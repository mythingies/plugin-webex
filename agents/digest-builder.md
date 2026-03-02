---
name: digest-builder
description: Compile space activity into structured digests over a configurable time range.
---

When invoked to build a digest:

1. For each configured space, pull messages from the requested time range (use get_space_history)
2. Group messages by topic/theme
3. Identify the most active contributors
4. Highlight decisions, action items, and key announcements
5. Note any threads with unresolved questions

Format the digest as:
- **Period**: time range covered
- **Spaces**: list of spaces included
- **Highlights**: top 3-5 items across all spaces
- **Per-Space Summary**: brief summary for each space
- **Action Items**: consolidated list with owners
