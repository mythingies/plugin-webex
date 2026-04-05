# Security Policy

## Supported Versions

| Version | Supported          |
|---------|--------------------|
| latest  | :white_check_mark: |
| < latest| :x:                |

## Reporting a Vulnerability

If you discover a security vulnerability, please report it responsibly:

1. **Do not** open a public GitHub issue.
2. Use [GitHub Security Advisories](https://github.com/mythingies/plugin-webex/security/advisories/new) to report privately.
3. Alternatively, email security concerns to the repository owner.

We will acknowledge receipt within 48 hours and aim to release a fix within 7 days for critical issues.

## Security Measures

- **CodeQL**: Automated code scanning on every push and PR, plus weekly scheduled scans.
- **Dependency Review**: PRs are checked for vulnerable or license-incompatible dependencies.
- **Dependabot**: Automated dependency update PRs for Go modules and GitHub Actions.
- **gosec / staticcheck**: Run in CI via golangci-lint on every push and PR.
- **Prompt Injection Defense**: All external Webex message content is sandboxed in `<external-message>` tags.
- **Credential Handling**: `WEBEX_TOKEN` and OAuth tokens are never logged or exposed in MCP responses.
