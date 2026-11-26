package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"syscall"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
)

// messageRoute is the control route the message verb calls.
const messageRoute = "/runs/message"

// runMessageMain implements `freesided message -db <database> -run <id>
// -invocation <id> [-text <text>]` (issue #3632): it sends an operator's
// message to a running cordoned writer through the running daemon, as a
// client's send_writer_message command does, and prints the message's
// record. With no -text it reads the text from stdin.
//
// It needs a running daemon and never opens the store itself: only the
// running daemon's driver can offer the message to the run.
func runMessageMain(args []string) {
	cfg, err := parseMessageCommand(args, os.Stdin, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided message:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	record, err := runMessageCommand(ctx, cfg)
	if err == nil {
		err = json.NewEncoder(os.Stdout).Encode(record)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
}

// messageCommandConfig is also the messageRoute control payload.
type messageCommandConfig struct {
	DBPath       string              `json:"db_path"`
	RunID        domain.RunID        `json:"run_id"`
	InvocationID domain.InvocationID `json:"invocation_id"`
	Text         string              `json:"text"`
}

func parseMessageCommand(args []string, stdin io.Reader, stderr io.Writer) (messageCommandConfig, error) {
	flags := flag.NewFlagSet("freesided message", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dbPath := flags.String("db", "", "SQLite database path of the running daemon (required)")
	run := flags.String("run", "", "run whose writer gets the message (required)")
	invocation := flags.String("invocation", "", "the writer invocation (required)")
	text := flags.String("text", "", "the message; read from stdin when absent")
	if err := flags.Parse(args); err != nil {
		return messageCommandConfig{}, err
	}
	if flags.NArg() != 0 {
		return messageCommandConfig{}, fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	cfg := messageCommandConfig{
		DBPath: *dbPath, RunID: domain.RunID(*run), InvocationID: domain.InvocationID(*invocation), Text: *text,
	}
	textSet := false
	flags.Visit(func(f *flag.Flag) { textSet = textSet || f.Name == "text" })
	if !textSet {
		// One byte past the bound, so an oversized message is refused by
		// the record's own check rather than cut short here.
		read, err := io.ReadAll(io.LimitReader(stdin, domain.MaxOperatorMessageBytes+1))
		if err != nil {
			return messageCommandConfig{}, fmt.Errorf("read the message from stdin: %w", err)
		}
		cfg.Text = string(read)
	}
	if err := validateMessageConfig(cfg); err != nil {
		return messageCommandConfig{}, err
	}
	return cfg, nil
}

func validateMessageConfig(cfg messageCommandConfig) error {
	switch {
	case cfg.DBPath == "":
		return errors.New("-db is required")
	case cfg.RunID == "":
		return errors.New("-run is required")
	case cfg.InvocationID == "":
		return errors.New("-invocation is required")
	default:
		return domain.ValidateOperatorMessageText(cfg.Text)
	}
}

// runMessageCommand forwards the message to the daemon that holds
// cfg.DBPath.
func runMessageCommand(ctx context.Context, cfg messageCommandConfig) (domain.OperatorMessage, error) {
	if err := validateMessageConfig(cfg); err != nil {
		return domain.OperatorMessage{}, fmt.Errorf("message: %w", err)
	}
	access, err := openStoreOrControl(cfg.DBPath)
	if err != nil {
		return domain.OperatorMessage{}, fmt.Errorf("message: %w", err)
	}
	defer func() { _ = access.Close() }()
	if access.client == nil {
		// The lock was free, so no daemon runs; release it unopened.
		return domain.OperatorMessage{}, fmt.Errorf(
			"message: no daemon is running on %s; a message reaches only a running writer", cfg.DBPath)
	}
	cfg.DBPath = access.client.dbPath
	var record domain.OperatorMessage
	if err := access.client.call(ctx, messageRoute, cfg, &record); err != nil {
		return domain.OperatorMessage{}, err
	}
	return record, nil
}

// sendMessageInDaemon sends the message inside the running daemon. It skips
// the client path's active-device gate: no paired device sent it, and the
// control socket's peer check already limits callers to the daemon's
// account, which the record names as cli:<account>, as freesided stop does.
func sendMessageInDaemon(
	ctx context.Context, attention *signet.Service, cfg messageCommandConfig,
) (domain.OperatorMessage, error) {
	account, err := user.Current()
	if err != nil {
		return domain.OperatorMessage{}, fmt.Errorf("message: read account: %w", err)
	}
	record, err := attention.SendOperatorMessage(ctx, domain.OperatorMessageRequest{
		CommandID: "cli-message-" + rand.Text(), Sender: "cli:" + account.Username,
		RunID: cfg.RunID, InvocationID: cfg.InvocationID, Text: cfg.Text,
	})
	if err != nil {
		return domain.OperatorMessage{}, fmt.Errorf("message: invocation %s of run %s: %w", cfg.InvocationID, cfg.RunID, err)
	}
	return record, nil
}
