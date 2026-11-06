package proxy

import (
	"encoding/json"
	"unicode/utf8"
)

// EstimateBodyTokens approximates the prompt tokens in a JSON request body.
//
// A raw len(body)/4 charges agent payloads too much: JSON quoting, keys and
// escaping inflate the byte count, so a 1 MB tool-loop body looked like a
// quarter-million tokens and ate a whole TPM cell. This walks the request's
// text instead: ASCII at roughly four characters per token, non-ASCII at
// roughly one and a half, a small per-message overhead, and flat estimates
// for media. Non-JSON bodies fall back to the byte count.
func EstimateBodyTokens(body []byte) int64 {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return int64(len(body) / 4)
	}

	var total int64
	if raw, ok := root["messages"]; ok {
		total += messageTokens(raw)
	}
	if raw, ok := root["input"]; ok {
		total += valueTokens(raw)
	}
	if raw, ok := root["tools"]; ok {
		total += int64(len(raw) / 4)
	}
	if total <= 0 {
		total = int64(len(body) / 4)
	}
	return total
}

// messageTokens walks a chat messages array. JSON structure and keys are not
// sent as tokens in this shape, so only content and tool calls are counted.
func messageTokens(raw json.RawMessage) int64 {
	var messages []json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return int64(len(raw) / 4)
	}
	var total int64
	for _, m := range messages {
		total += 4 // role framing and delimiters
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(m, &msg); err != nil {
			total += int64(len(m) / 4)
			continue
		}
		if content, ok := msg["content"]; ok {
			total += contentTokens(content)
		}
		if calls, ok := msg["tool_calls"]; ok {
			total += valueTokens(calls)
		}
	}
	return total
}

// contentTokens counts a message content field, which is either a string or a
// multi-part array.
func contentTokens(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return textTokens(s)
	}

	var parts []struct {
		Type       string          `json:"type"`
		Text       string          `json:"text"`
		ImageURL   json.RawMessage `json:"image_url"`
		InputAudio *struct {
			Data string `json:"data"`
		} `json:"input_audio"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return int64(len(raw) / 4)
	}

	var total int64
	for _, p := range parts {
		switch p.Type {
		case "text":
			total += textTokens(p.Text)
		case "image_url", "input_image":
			// Vision token cost depends on resolution, which is not in the
			// request. A flat band beats treating base64 bytes as text.
			if containsVideoMedia(p.ImageURL) {
				total += 4000
			} else {
				total += 1000
			}
		case "input_audio":
			// Compressed audio is roughly one token per few hundred bytes.
			if p.InputAudio != nil {
				total += int64(len(p.InputAudio.Data)/4000) + 1
			}
		default:
			total += int64(len(raw)) / 4 / int64(max(1, len(parts)))
		}
	}
	return total
}

// valueTokens counts any JSON value, treating a plain string as text and
// anything else by its byte size.
func valueTokens(raw json.RawMessage) int64 {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return textTokens(s)
	}
	return int64(len(raw) / 4)
}

// textTokens counts a text run with the usual two bands: ASCII prose averages
// about four characters per token, while non-ASCII (CJK, emoji) averages close
// to one and a half bytes per byte-pair token.
func textTokens(s string) int64 {
	var ascii, nonASCII int64
	for _, r := range s {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			nonASCII++
		}
	}
	total := ascii/4 + nonASCII*2/3
	if total == 0 && (ascii > 0 || nonASCII > 0) {
		return 1
	}
	return total
}

// containsVideoMedia reports whether an image_url part carries a video data
// URI, which some clients use because OpenAI has no video part type.
func containsVideoMedia(raw json.RawMessage) bool {
	return len(raw) > 0 && containsFold(string(raw), "data:video/")
}

// containsFold is a small case-insensitive substring check, avoiding a
// strings.ToLower allocation on every part.
func containsFold(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			a, b := haystack[i+j], needle[j]
			if 'A' <= a && a <= 'Z' {
				a += 'a' - 'A'
			}
			if 'A' <= b && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
