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
	"strconv"
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
	// (same posture as the pairing key). It must carry at least 32 bytes:
	// hosted ntfy topics are capability URLs, so they must be unguessable.
	// Pairing returns the derived topic only to that new device; Device and the
	// sync surfaces never carry it.
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

// NtfySubscription is the private device-scoped read capability returned by
// pairing. It deliberately carries no publish token: capability-topic
// deployments need only this base URL and topic, while authenticated ntfy
// subscriptions require a future device-scoped credential design rather than
// leaking the daemon's publisher credential.
type NtfySubscription struct {
	ServerURL string `json:"server_url"`
	Topic     string `json:"topic"`
}

// validate reports whether the channel can publish and issue private
// subscriptions safely; SubmitDelivery and Pair both fail closed on it before
// any write. A remote cleartext base would expose the capability topic even
// without a publisher token, while URL credentials would be copied into the
// pairing grant, so both are refused outright.
func (c *ntfyChannel) validate() error {
	base, err := parseHTTPURL(c.cfg.BaseURL)
	if err != nil {
		return fmt.Errorf("ntfy base URL: %w", err)
	}
	if _, err := parseHTTPURL(c.cfg.ClickBaseURL); err != nil {
		return fmt.Errorf("ntfy click base URL: %w", err)
	}
	if len(c.cfg.TopicKey) < sha256.Size {
		return fmt.Errorf("ntfy topic key is %d bytes, want at least %d", len(c.cfg.TopicKey), sha256.Size)
	}
	if base.Scheme != "https" && !isLoopbackHost(base.Hostname()) {
		return errors.New("ntfy capability topic over cleartext non-loopback HTTP")
	}
	return nil
}

// ntfySubscription returns exactly the address SubmitDelivery publishes to
// for id. Pairing fails before consuming its code when the channel is absent
// or invalid, so a successful grant never strands a device with an unusable
// subscription.
func (s *Service) ntfySubscription(id domain.DeviceID) (NtfySubscription, error) {
	if s.ntfy == nil {
		return NtfySubscription{}, errors.New("ntfy subscription: channel is not configured")
	}
	if err := s.ntfy.validate(); err != nil {
		return NtfySubscription{}, fmt.Errorf("ntfy subscription: %w", err)
	}
	base, _ := parseHTTPURL(s.ntfy.cfg.BaseURL) // validate above proved this parse succeeds.
	return NtfySubscription{
		ServerURL: strings.TrimRight(base.String(), "/"),
		Topic:     s.ntfy.topic(id),
	}, nil
}

// parseHTTPURL accepts only a credential-free absolute http(s) base URL with
// a host. Query and fragment syntax cannot be part of a base: string-appending
// a topic or item path after either would route somewhere other than the
// returned subscription or Click target.
func parseHTTPURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("invalid URL syntax")
	}
	if u.User != nil {
		return nil, errors.New("URL must not include userinfo")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("URL is not an absolute http(s) URL")
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return nil, errors.New("URL port is outside 1-65535")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, errors.New("URL port is empty")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return nil, errors.New("URL must not include a query")
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return nil, errors.New("URL must not include a fragment")
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
// reason text, a deep link to the canonical item. The link carries the
// delivery's channel and attempt as query parameters — the GET it targets
// stays side-effect-free, and the client derives the exact opened-receipt
// PUT from them (#130). The provider-visible metadata surface is the item ID
// and the attempt counter (inside the link) plus the priority; the widening
// from #69's item-ID-and-priority surface is an owner decision (decision
// note 2026-07-16-2038).
func (c *ntfyChannel) notificationFor(item domain.AttentionItem, device domain.DeviceID, attempt int) notification {
	return notification{
		topic: c.topic(device),
		title: "Attention needed",
		body:  strings.ReplaceAll(string(item.Type), "_", " "),
		click: strings.TrimRight(c.cfg.ClickBaseURL, "/") + "/attention/items/" + url.PathEscape(string(item.ID)) +
			"?channel=" + url.QueryEscape(channelNtfy) + "&attempt=" + strconv.Itoa(attempt),
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
