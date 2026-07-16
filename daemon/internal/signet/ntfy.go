package signet

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ntfy is the Phase 1 notification channel (plan §10 defaults to the hosted
// service). Notifications are read-only hints (plan §4): a generic title, the
// attention type, and a Click deep link into canonical state. Item subject
// and reason text never leave the daemon — on hosted ntfy the payload
// transits a third party, so the hint stays generic by owner decision (the
// unit's devlog note); everything real is behind the deep link.

// channelNtfy is the delivery rows' channel key; test fixtures and goldens
// across domain and store already use this literal.
const channelNtfy = "ntfy"

// ErrChannelRejected is the class sentinel for a notification the channel
// provider did not accept; it is carried by *ChannelRejectionError. Match the
// class with errors.Is and recover the status with errors.As.
var ErrChannelRejected = errors.New("channel did not accept the notification")

// ChannelRejectionError reports a non-2xx channel response. It carries the
// status code only — never the response body, which is third-party content
// and could echo credential material into logs.
type ChannelRejectionError struct {
	Status int
}

func (e *ChannelRejectionError) Error() string {
	return fmt.Sprintf("ntfy returned status %d", e.Status)
}

// Is lets errors.Is(err, ErrChannelRejected) match the class while errors.As
// recovers the status.
func (e *ChannelRejectionError) Is(target error) bool { return target == ErrChannelRejected }

// NtfyConfig composes the ntfy channel. BaseURL and TopicKey are required;
// ClickBaseURL is required because a notification without its deep link into
// canonical state would invite the client to act on the hint itself, exactly
// what "notifications are read-only hints" forbids.
type NtfyConfig struct {
	// BaseURL is the ntfy server, e.g. https://ntfy.sh for the hosted default.
	BaseURL string
	// Client is the outbound HTTP client; nil gets a private client with a
	// timeout, so an unresponsive provider cannot hang the pipeline forever.
	Client *http.Client
	// Token is the optional ntfy access token; it is revealed only into the
	// Authorization header.
	Token Secret
	// TopicKey is the daemon-held key per-device topics are derived under
	// (same posture as the pairing key): hosted ntfy topics are capability
	// URLs, so they must be unguessable, and Device has no topic field —
	// surfacing the topic to the device is a deferred contract unit.
	TopicKey []byte
	// ClickBaseURL is the deep-link base the Click header points at; the
	// daemon API origin in Phase 1.
	ClickBaseURL string
}

// WithNtfy supplies the ntfy notification channel. Without it, or with an
// incomplete config, delivery submission fails closed.
func WithNtfy(cfg NtfyConfig) Option {
	return func(s *Service) { s.ntfy = &ntfyChannel{cfg: cfg} }
}

type ntfyChannel struct {
	cfg NtfyConfig
}

// validate reports whether the channel can publish at all; SubmitDelivery
// fails closed on it before any write, so a permanently broken config can
// never burn a submitted-only row per call. A token on a cleartext non-local
// base is refused outright: the only Reveal site is the Authorization header,
// and sending it over plain HTTP off-host would leak the credential on every
// publish.
func (c *ntfyChannel) validate() error {
	base, err := parseHTTPURL(c.cfg.BaseURL)
	if err != nil {
		return fmt.Errorf("ntfy base URL: %w", err)
	}
	if _, err := parseHTTPURL(c.cfg.ClickBaseURL); err != nil {
		return fmt.Errorf("ntfy click base URL: %w", err)
	}
	if len(c.cfg.TopicKey) == 0 {
		return errors.New("ntfy topic key is empty")
	}
	if c.cfg.Token.Reveal() != "" && base.Scheme != "https" && !isLoopbackHost(base.Hostname()) {
		return errors.New("ntfy token over cleartext non-loopback HTTP")
	}
	return nil
}

// parseHTTPURL accepts only an absolute http(s) URL with a host.
func parseHTTPURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%q is not an absolute http(s) URL", raw)
	}
	return u, nil
}

// isLoopbackHost reports whether host names the local machine, where a
// cleartext bearer token never crosses a network (the dev harness and tests
// compose exactly this).
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// topic derives the device's notification topic: an HMAC of the device ID
// under the daemon-held key, truncated to 32 hex characters (128 bits). The
// derivation is deterministic (no schema needed) and the result unguessable
// without the key, which is what a capability-URL topic requires.
func (c *ntfyChannel) topic(id domain.DeviceID) string {
	mac := hmac.New(sha256.New, c.cfg.TopicKey)
	mac.Write([]byte(id))
	return "fs-" + hex.EncodeToString(mac.Sum(nil))[:32]
}

// notification is the read-only hint published for one delivery attempt.
type notification struct {
	topic    string
	title    string
	body     string
	click    string
	priority domain.Priority
}

// notificationFor renders the generic hint for item to device: no subject or
// reason text, a deep link to the canonical item.
func (c *ntfyChannel) notificationFor(item domain.AttentionItem, device domain.DeviceID) notification {
	return notification{
		topic:    c.topic(device),
		title:    "Attention needed",
		body:     strings.ReplaceAll(string(item.Type), "_", " "),
		click:    strings.TrimRight(c.cfg.ClickBaseURL, "/") + "/attention/items/" + url.PathEscape(string(item.ID)),
		priority: item.Priority,
	}
}

// publish posts one notification. A 2xx is the provider's acceptance — and
// nothing stronger: the caller records channel_accepted_at, never
// "delivered". The response body is drained and discarded, mirroring the
// publish package's outbound discipline.
func (c *ntfyChannel) publish(ctx context.Context, n notification) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.cfg.BaseURL, "/")+"/"+url.PathEscape(n.topic), strings.NewReader(n.body))
	if err != nil {
		return fmt.Errorf("ntfy: build request: %w", err)
	}
	req.Header.Set("Title", n.title)
	req.Header.Set("Click", n.click)
	req.Header.Set("Priority", ntfyPriority(n.priority))
	if token := c.cfg.Token.Reveal(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := c.cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("ntfy: %w", &ChannelRejectionError{Status: resp.StatusCode})
	}
	return nil
}

// drainAndClose discards at most a small remainder so the connection can be
// reused, then closes. Response bodies are never retained or surfaced: an
// error body is third-party content and can echo credential material.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4096))
	_ = body.Close()
}

// ntfyPriority maps the item's priority onto ntfy's named levels. A behaviour
// switch, no default: a new Priority member must choose its notification
// urgency here. The invalid zero value falls back to the provider default.
func ntfyPriority(p domain.Priority) string {
	switch p {
	case domain.PriorityLow:
		return "low"
	case domain.PriorityNormal:
		return "default"
	case domain.PriorityHigh:
		return "high"
	case domain.PriorityUrgent:
		return "urgent"
	}
	return "default"
}
