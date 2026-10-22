# Sync browser chat history

In Import, select Claude.ai, ChatGPT, Gemini, or Grok, then click Sync.
Chrome must stay open with the AgentsView Browser Sync extension enabled in
one profile. Sign in to the selected service in that profile.

After upgrading from the ChatGPT-only extension, run `agentsview chrome setup`,
restart AgentsView, and reload the extension at `chrome://extensions`. Protocol
3 adds the Gemini and Grok read paths. All four platforms use the same native
relay and local-only server checks.

The CLI supports the same operations:

```sh
agentsview import --type gemini --sync
agentsview import --type grok --sync
```

The server exposes `POST /api/v1/import/gemini/sync` and
`POST /api/v1/import/grok/sync`. Both stream progress, completion counts, and
recoverable sign-in errors. Only local connections may use these routes.

## What Gemini and Grok currently import

- Text conversations. Attachments, generated images, and usage accounting are
  not imported. Gemini turns without a native timestamp remain undated.
- Gemini checks both pinned and unpinned conversation lists. It follows list
  cursors and increases the requested history size when a chat fills a read.
  It refuses a chat that still reports more history at 12,800 turns.
- Grok follows list cursors, loads message bodies in batches of 100, and
  reconstructs a single complete parent chain. Branched chats, missing bodies,
  unfinished responses, unknown roles, and malformed timestamps are refused.
- Each run re-reads the text, because a matching list/detail revision has not
  been verified for these services. Identical content skips writes. Requests
  are paced about three seconds apart; retries honor bounded rate-limit waits.

## Rules for existing archives

New browser session IDs use `gemini-apps:web:<native-id>` and
`grok:web:<native-id>`. They do not replace Gemini Takeout activities or local
Grok agent sessions. Importing the same conversation from Takeout and the
browser can therefore leave separate records.

Changed browser text keeps its previous version in Trash. Malformed or
incomplete reads never replace saved messages and are retried on the next
Sync. Trash and exclusion markers are respected. Cancellation keeps committed
chats and stops further requests. No live data migration is required.

These web interfaces are undocumented and can change. Synthetic tests verify
the relay, parsing, persistence, and failure rules; they do not establish
compatibility with a currently signed-in account. Before enabling these
providers on a live archive, use a scratch archive and compare a short chat,
a long chat, a pinned Gemini chat, and Grok's refusal of a branch against the
websites. Repeat Sync and inspect Trash after editing a chat.
