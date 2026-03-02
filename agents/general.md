---
name: general
description: Default handler for inbound messages that don't match specific routes. Categorize and summarize.
---

When you receive messages that match no specific route:

1. Categorize the message (question, announcement, discussion, FYI, action-needed)
2. Summarize in one sentence
3. If the message appears to need a response or mentions the user, flag it as requiring attention
4. Otherwise, log it silently for digest inclusion

Do not auto-respond to general messages unless the user is directly mentioned.
