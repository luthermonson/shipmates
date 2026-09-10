package discord

import (
	"regexp"
	"strings"

	"github.com/luthermonson/shipmates/internal/bridge"
)

// This file holds the two trust boundaries of the transport, both as pure
// functions so they can be tested without a live Discord connection.
//
//   - sanitizeInbound: Discord -> mate. Hostile text becomes bounded tell
//     content and nothing more.
//   - stripMentions / sanitizeOutbound: mate -> Discord. Mate output cannot
//     ping real people, and cannot carry terminal escape sequences onto a
//     surface that renders them.

// sanitizeInbound turns a raw Discord message into the content of a tell. The
// text is DATA, never instructions: this function only trims surrounding
// whitespace and enforces a hard rune bound. It does not, and must not, try to
// interpret slash-commands, mentions, or any other structure — the mate
// receives exactly bounded text.
//
// It returns the cleaned content and ok=false when nothing dispatchable
// remains (empty or whitespace-only), so the caller drops it rather than
// sending an empty tell. Over-long input is truncated to MaxInboundRunes rather
// than rejected, so a long paste still lands as bounded content.
func sanitizeInbound(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	runes := []rune(s)
	if len(runes) > MaxInboundRunes {
		runes = runes[:MaxInboundRunes]
		s = strings.TrimSpace(string(runes))
		if s == "" {
			return "", false
		}
	}
	return s, true
}

// userRoleMention matches a Discord user or role mention: <@123>, <@!123>
// (nickname form), and <@&456> (role). These are the tokens that ping a
// specific person or role.
var userRoleMention = regexp.MustCompile(`<@[!&]?\d+>`)

// broadcastMention matches the two channel-wide pings, @everyone and @here,
// which notify every member of a server.
var broadcastMention = regexp.MustCompile(`@(everyone|here)`)

// stripMentions neutralizes every Discord mention in a string so mate output
// cannot ping real people. It works structurally rather than with invisible
// characters (which the terminal-escape scrubber would later strip anyway):
//   - user/role mentions become the literal text "[mention]";
//   - @everyone / @here lose their leading @, becoming plain words.
//
// This is the first of two outbound layers; the second is discordgo's
// AllowedMentions set to none on the actual send (see transport.go).
func stripMentions(s string) string {
	s = userRoleMention.ReplaceAllString(s, "[mention]")
	s = broadcastMention.ReplaceAllString(s, "$1")
	return s
}

// sanitizeOutbound prepares mate text for posting to Discord: mentions are
// neutralized, then the result is run through the terminal-escape scrubber
// (bridge.Chrome) since mate output can carry escape sequences, and bounded to
// MaxOutboundCells so it fits inside Discord's message limit. Returns ok=false
// when nothing renderable remains.
//
// Chrome treats newlines as C0 controls and drops them, which would smash a
// multi-line reply into one run. To keep replies readable we scrub each line
// independently (so escape sequences within a line are still removed as units)
// and rejoin with '\n', then enforce the total cell bound with a final Chrome
// pass would re-drop the newlines — so the bound is applied by truncation here.
func sanitizeOutbound(raw string) (string, bool) {
	stripped := stripMentions(raw)
	lines := strings.Split(stripped, "\n")
	cleaned := make([]string, 0, len(lines))
	for _, ln := range lines {
		// max<=0: scrub fully without a per-line length bound; the total bound
		// is enforced below across the whole message.
		cleaned = append(cleaned, bridge.Chrome(ln, 0))
	}
	joined := strings.TrimSpace(strings.Join(cleaned, "\n"))
	if joined == "" {
		return "", false
	}
	if runes := []rune(joined); len(runes) > MaxOutboundCells {
		joined = strings.TrimSpace(string(runes[:MaxOutboundCells-1])) + "…"
	}
	return joined, true
}
