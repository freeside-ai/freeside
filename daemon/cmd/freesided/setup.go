package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

func runSetupMain(args []string) {
	if err := runSetup(context.Background(), args, os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided setup:", err)
		os.Exit(1)
	}
}

type setupResult struct {
	operations.Layout
	Status         string                   `json:"status"`
	Registration   *publish.AppRegistration `json:"registration,omitempty"`
	ManifestAction string                   `json:"manifest_action,omitempty"`
	ManifestFields url.Values               `json:"manifest_fields,omitempty"`
}

func runSetup(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return runSetupCommand(
		ctx, args, stdin, stdout, stderr,
		&http.Client{Timeout: 30 * time.Second},
		defaultGitHubAPIBase,
		defaultGitHubRemoteBase,
	)
}

func runSetupCommand(
	ctx context.Context,
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	client *http.Client,
	apiBaseURL, webBaseURL string,
) error {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-registration-code-stdin=") ||
			strings.HasPrefix(arg, "--registration-code-stdin=") {
			return errors.New("-registration-code-stdin does not accept a value")
		}
	}
	flags := flag.NewFlagSet("freesided setup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configDir := flags.String("config-dir", "", "single owner-private Freeside configuration directory")
	operator := flags.String("operator", "", "canonical personal GitHub login (required)")
	operatorID := flags.Int64("operator-id", 0, "canonical numeric personal GitHub account ID (required)")
	registrationCodeStdin := flags.Bool(
		"registration-code-stdin", false,
		"read the one-time GitHub App manifest conversion code from standard input")
	registrationRetry := flags.Int(
		"registration-retry", 0,
		"App-name collision retry number used to regenerate the manifest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %d", flags.NArg())
	}
	if *operator == "" || *operatorID <= 0 {
		return errors.New("-operator and positive -operator-id are required")
	}
	if *registrationRetry < 0 {
		return errors.New("-registration-retry must not be negative")
	}
	layout, err := operations.Setup(ctx, *configDir)
	if err != nil {
		return err
	}
	keystore, err := publish.NewKeystore(layout.CredentialsDir, layout.StateDir)
	if err != nil {
		return err
	}
	authority, err := publish.NewInstallationAuthorityStore(layout.StateDir)
	if err != nil {
		return err
	}
	apps, err := keystore.ListAppsIncludingPendingAuthority()
	if err != nil {
		return err
	}
	selectedApp := -1
	for i, app := range apps {
		registration := app.Registration()
		if registration.OwnerID != *operatorID {
			continue
		}
		if selectedApp >= 0 {
			return publish.ErrAmbiguousAppRegistration
		}
		selectedApp = i
	}
	if selectedApp < 0 && len(apps) != 0 {
		return errors.New("existing GitHub App registrations do not match the operator")
	}
	if len(apps) == 0 {
		if err := authority.InitializeDocument(ctx); err != nil {
			return err
		}
	}
	result := setupResult{Layout: layout}
	if selectedApp >= 0 {
		app := apps[selectedApp]
		registration := app.Registration()
		if !strings.EqualFold(registration.Owner, *operator) {
			return errors.New("existing GitHub App registration does not match the operator")
		}
		document, err := authority.Document(ctx)
		if err != nil {
			return err
		}
		found := false
		for _, entry := range document.Registrations {
			if entry.RegistrationID == registration.AppID {
				for _, owner := range entry.TrustedOwners {
					if owner.ID == registration.OwnerID &&
						strings.EqualFold(owner.Login, registration.Owner) {
						found = true
					}
				}
			}
		}
		if !found {
			if !app.AuthorityPending {
				return errors.New(
					"existing GitHub App credentials have no matching owner authority; refusing unmarked credentials")
			}
			// The marker was atomically persisted with the one-time key, so
			// setup can finish only that exact interrupted conversion into
			// its matching authority registration.
			if err := authority.InitializeRegistration(ctx, registration); err != nil {
				return err
			}
		}
		if err := keystore.FinalizeAppAuthority(registration); err != nil {
			return err
		}
		result.Status = "complete"
		result.Registration = &registration
	} else if *registrationCodeStdin {
		registrationCode, err := readRegistrationCode(stdin)
		if err != nil {
			return err
		}
		registrar := publish.NewRegistrar(keystore, client, apiBaseURL, webBaseURL)
		credentials, err := registrar.ExchangeCodePendingAuthority(
			ctx, registrationCode.Reveal(), publish.RegistrationTarget{
				Owner: *operator, OwnerID: *operatorID, Visibility: publish.AppVisibilityPublic,
			},
		)
		if err != nil {
			return err
		}
		registration := credentials.Registration()
		if err := authority.InitializeRegistration(ctx, registration); err != nil {
			return err
		}
		if err := keystore.FinalizeAppAuthority(registration); err != nil {
			return err
		}
		result.Status = "complete"
		result.Registration = &registration
	} else {
		registrar := publish.NewRegistrar(keystore, client, apiBaseURL, webBaseURL)
		action, fields, err := registrar.ManifestForm(
			publish.RegistrationTarget{
				Owner: *operator, OwnerID: *operatorID, Visibility: publish.AppVisibilityPublic,
			},
			*operator,
			*registrationRetry,
			publish.Manifest{URL: defaultGitHubRemoteBase + "/freeside-ai/freeside"},
		)
		if err != nil {
			return err
		}
		result.Status = "registration_required"
		result.ManifestAction = action
		result.ManifestFields = fields
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("write setup result: %w", err)
	}
	return nil
}

const maxRegistrationCodeBytes = 4096

func readRegistrationCode(stdin io.Reader) (publish.Secret, error) {
	if stdin == nil {
		return "", errors.New("registration code standard input is unavailable")
	}
	payload, err := io.ReadAll(io.LimitReader(stdin, maxRegistrationCodeBytes+1))
	if err != nil {
		// Reader errors are untrusted: some implementations include recently
		// read bytes in the error, which would turn diagnostics into a leak.
		return "", errors.New("read registration code from standard input")
	}
	if len(payload) > maxRegistrationCodeBytes {
		return "", errors.New("registration code from standard input exceeds 4096 bytes")
	}
	code := strings.TrimSuffix(string(payload), "\n")
	code = strings.TrimSuffix(code, "\r")
	if code == "" {
		return "", errors.New("registration code from standard input is empty")
	}
	if strings.ContainsAny(code, "\r\n") {
		return "", errors.New("registration code from standard input must be one line")
	}
	return publish.Secret(code), nil
}
