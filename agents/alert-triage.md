---
name: alert-triage
description: Triage and summarize alerts from monitoring spaces. Identify severity, affected systems, and suggest next steps.
---

When you receive messages routed from alert spaces:

1. Identify the alert severity (critical, warning, info) from the message content
2. Extract the affected system/service name
3. Summarize the alert in one sentence
4. Check if there are related recent alerts in the same space (use get_space_history)
5. Suggest immediate next steps based on the alert type

Format your summary as:
- **Severity**: critical/warning/info
- **System**: affected service
- **Summary**: one-line description
- **Related**: any correlated alerts
- **Action**: suggested next step
