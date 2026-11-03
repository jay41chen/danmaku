# 0006 — Text input validation: reject, don't rewrite

**Status**: Accepted (2026-11-03)

## Context

`strings.TrimSpace` doesn't recognize a zero-width space: U+200B is in category Cf, not a space. That made
invisible messages possible. Room names could also contain newlines, an RLO character (U+202E, which
displays the text reversed), or Zalgo text, where dozens of combining marks are stacked on one letter.

- A **whitelist** (letters, digits, CJK) rejects too much: full-width punctuation, Bopomofo, emoji, kaomoji,
  kana.
- A **blacklist** by category fails open, and also blocks legitimate characters: ZWJ, which joins emoji
  sequences, is Cf, and Vietnamese needs combining marks (Mn).

## Decision

Split the problem by layer:

| Problem | Layer | How |
|---|---|---|
| Not valid text (control, zero-width, bidi override) | Input (server) | **PRECIS FreeformClass, RFC 8264** (`golang.org/x/text/secure/precis`) |
| Layout (Zalgo, overlong, RLO affecting neighbours) | Output (client) | `<bdi>` isolation + `overflow: hidden` |
| Impersonation (`Ａdmin` vs `Admin`) | Comparison | NFKC before comparing; only for unique identifiers |
| Abuse (profanity, spam) | Moderation (later) | Normalize *then* match, so zero-width tricks don't bypass it |

- **Only check, never rewrite** (OWASP: reject, don't sanitize). Only the PRECIS *error* is used; its output
  is thrown away and the original string is stored. Rewriting changes what the user sent, and the rewrite
  itself can be abused, because what's left after removing something can spell what was blocked.
  - Comparing the output with the input doesn't work either: the profile normalizes `！` to `!` and NFD
    `café` to NFC, so ordinary input would be rejected.
- **Emoji with VS16.** ❤️ is U+2764 + U+FE0F, which is exactly what mobile keyboards send, and PRECIS
  rejects U+FE0F by default. Rather than special-casing it in the validator, the profile uses PRECIS's own
  extension points: `AdditionalMapping` strips VS15/VS16 before checking, and `DisallowEmpty` rejects input
  that consisted only of selectors.
- `AdditionalMapping` takes a **factory** (`func() transform.Transformer`) because transformers are
  stateful; they are built for streaming. The profile is shared by every request, so each call must get a
  fresh transformer. The general rule: to share something stateful, share the way to make it.
- Room names are stricter than messages, because they appear in the public room list: no leading or
  trailing whitespace, no consecutive whitespace, and at most 2 consecutive combining marks (Mn + Me).
  Vietnamese needs up to 2, and the keycap emoji 1️⃣ uses exactly 2.
- Validators return package-level sentinel errors whose text is sent to the client as-is. Internal errors
  (precis, SQL) are never passed through.

## Consequences

- Known gaps: ZWJ emoji sequences (👨‍👩‍👧) are rejected by a PRECIS contextual rule. Newly added Unicode
  emoji are rejected until `x/text` is upgraded. Characters that look blank but aren't Unicode whitespace
  (such as U+2800) belong to the moderation layer.
- Existing data was not cleaned up retroactively.
