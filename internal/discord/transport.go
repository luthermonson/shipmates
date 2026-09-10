package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/luthermonson/shipmates/internal/client"
	"github.com/luthermonson/shipmates/internal/personaname"
)

// This file is the only place discordgo and the ship's HTTP surface meet. The
// two directions each reuse the EXISTING tell/events seam through
// internal/client — the same Get/Post helpers the CLI uses, which attach the
// per-run captain bearer token (project.ReadAPIToken) on every request. No new
// tell/events plumbing is invented here; this is a second front-end onto the
// same loop the voice conversation in internal/fleet drives.

// shipClient is the seam into the captain's local API. Concretely it is
// internal/client's Get/Post (loopback + Authorization: Bearer <server.token>);
// the interface keeps the transport testable and pins the exact two calls this
// transport makes against the authenticated API.
type shipClient interface {
	Get(path string) ([]byte, error)
	Post(path string, body any) ([]byte, error)
}

// clientSeam adapts internal/client's package-level Get/Post to shipClient.
type clientSeam struct{}

func (clientSeam) Get(path string) ([]byte, error)            { return client.Get(path) }
func (clientSeam) Post(path string, body any) ([]byte, error) { return client.Post(path, body) }

// event mirrors the subset of internal/server.Event we consume from GET
// /events. Time is an opaque, lexically-sortable timestamp string, used only as
// a high-water mark (the same treatment internal/fleet's wait_for_result gives
// it).
type event struct {
	Time    string `json:"time"`
	Persona string `json:"persona"`
	Type    string `json:"type"`
	Text    string `json:"text"`
}

// Transport is one Discord bot bound to one channel, bridging a single persona.
type Transport struct {
	cfg  Config
	ship shipClient
	sess *discordgo.Session

	// lastSeen is the high-water mark: the greatest event Time already posted
	// outbound. Primed to "now's newest" at start so history is not replayed.
	lastSeen string
}

// New builds a Transport from an operator-supplied Config, wired to the real
// ship client seam.
func New(cfg Config) *Transport {
	return &Transport{cfg: cfg, ship: clientSeam{}}
}

// Run connects to Discord, registers the inbound handler, and drives the
// outbound poll loop until ctx is cancelled. The bot token is read from the
// environment HERE, immediately before use, and handed to discordgo; it is
// never stored on Config, never returned, and never logged.
func (t *Transport) Run(ctx context.Context) error {
	// Validate the operator-supplied persona once, up front: it becomes a path
	// segment on every tell, so a bad value must fail loudly at startup rather
	// than per-message.
	if err := personaname.Validate(t.cfg.Persona); err != nil {
		return fmt.Errorf("discord: configured persona is invalid: %w", err)
	}

	tok, err := tokenFromEnv()
	if err != nil {
		return err
	}
	sess, err := discordgo.New("Bot " + tok)
	if err != nil {
		// discordgo.New does not echo the token in its errors, but keep the
		// message generic regardless — never risk surfacing the credential.
		return fmt.Errorf("discord: could not create session")
	}
	// MessageContent is a privileged intent required to read message text; the
	// guild-messages intent delivers the create events at all.
	sess.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentMessageContent
	sess.AddHandler(t.onMessageCreate)
	t.sess = sess

	if err := sess.Open(); err != nil {
		return fmt.Errorf("discord: could not open gateway connection")
	}
	defer sess.Close()
	slog.Info("discord transport connected", "channel", t.cfg.Channel, "persona", t.cfg.Persona)

	t.primeHighWater()

	ticker := time.NewTicker(t.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			t.pumpOutbound()
		}
	}
}

// onMessageCreate is the inbound trust boundary. It is deliberately terse and
// fail-closed: anything not explicitly permitted is dropped, and message
// content never becomes anything but bounded tell data.
func (t *Transport) onMessageCreate(_ *discordgo.Session, m *discordgo.MessageCreate) {
	if m == nil || m.Author == nil {
		return
	}
	// Never react to bots (including ourselves) — prevents an outbound post
	// from looping back in as a command.
	if m.Author.Bot {
		return
	}
	// Single bound channel only.
	if m.ChannelID != t.cfg.Channel {
		return
	}
	// Fail-closed allowlist. An unlisted author is read-only: the message is
	// dropped, never dispatched. Log the decision by author id only — never the
	// message content, and never anything that could carry the token.
	if !t.cfg.Allowed.Allows(m.Author.ID) {
		slog.Debug("discord: ignoring message from non-allowlisted user", "user", m.Author.ID)
		return
	}
	content, ok := sanitizeInbound(m.Content)
	if !ok {
		return
	}
	if err := t.dispatch(content); err != nil {
		slog.Warn("discord: tell dispatch failed", "user", m.Author.ID, "err", err)
		return
	}
	slog.Info("discord: dispatched tell", "user", m.Author.ID, "persona", t.cfg.Persona, "bytes", len(content))
}

// dispatch sends the sanitized content as a tell to the configured persona,
// reusing the authenticated POST /tell/{persona} seam. The persona is escaped
// into the path (it was already validated at startup).
func (t *Transport) dispatch(content string) error {
	path := "/tell/" + url.PathEscape(t.cfg.Persona)
	if _, err := t.ship.Post(path, map[string]string{"message": content}); err != nil {
		return err
	}
	return nil
}

// primeHighWater reads the current event log once and sets lastSeen to the
// newest event time, so the transport starts posting only replies produced
// AFTER it connected rather than replaying the whole history into the channel.
func (t *Transport) primeHighWater() {
	events, err := t.fetchEvents()
	if err != nil {
		slog.Warn("discord: could not prime event high-water mark", "err", err)
		return
	}
	for _, e := range events {
		if e.Time > t.lastSeen {
			t.lastSeen = e.Time
		}
	}
}

// pumpOutbound polls GET /events and posts any new assistant replies for the
// configured persona to the channel.
func (t *Transport) pumpOutbound() {
	events, err := t.fetchEvents()
	if err != nil {
		slog.Debug("discord: event poll failed", "err", err)
		return
	}
	newHigh := t.lastSeen
	for _, e := range events {
		if e.Time <= t.lastSeen {
			continue
		}
		if e.Time > newHigh {
			newHigh = e.Time
		}
		// Only the configured persona's spoken replies go to the channel.
		if e.Persona != t.cfg.Persona || e.Type != "assistant" || e.Text == "" {
			continue
		}
		out, ok := sanitizeOutbound(e.Text)
		if !ok {
			continue
		}
		if err := t.postToChannel(out); err != nil {
			slog.Warn("discord: outbound post failed", "err", err)
		}
	}
	t.lastSeen = newHigh
}

// fetchEvents pulls the captain's event log over the authenticated GET /events.
func (t *Transport) fetchEvents() ([]event, error) {
	body, err := t.ship.Get("/events")
	if err != nil {
		return nil, err
	}
	var events []event
	if err := json.Unmarshal(body, &events); err != nil {
		return nil, fmt.Errorf("decode events: %w", err)
	}
	return events, nil
}

// postToChannel posts already-sanitized text to the bound channel with
// AllowedMentions set to none — the belt-and-braces second layer behind
// stripMentions. A non-nil, empty Parse slice means Discord parses no mentions
// of any kind, so even if a token slipped past stripMentions it cannot ping.
func (t *Transport) postToChannel(content string) error {
	_, err := t.sess.ChannelMessageSendComplex(t.cfg.Channel, &discordgo.MessageSend{
		Content: content,
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse: []discordgo.AllowedMentionType{},
		},
	})
	return err
}
