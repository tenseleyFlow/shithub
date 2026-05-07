# Security policy

## Reporting a vulnerability

shithub is pre-launch. The project does not yet have a dedicated security mailbox. For now, please open a private channel of communication with the maintainer (contact via GitHub) before disclosing publicly.

Once shithub launches at its public domain, this policy will be updated with:

- A dedicated `security@<domain>` mailbox
- A PGP public key for sensitive reports
- A response-time SLO (target: 72 hours initial acknowledgement)
- A scope statement covering the hosted instance plus the self-hosted code
- A coordinated-disclosure timeline

## Out of scope (pre-launch)

- Findings against unreleased / pre-launch builds in development environments
- Issues that require a foothold the maintainer's machine to exploit
- Theoretical findings without a working proof of concept

## In scope (once launched)

- Authentication / authorization bypasses
- Server-side request forgery
- Code injection (SQL, template, command, etc.)
- Cross-site scripting and CSRF
- Insecure cryptographic practices
- Resource exhaustion / denial-of-service vectors
- Information disclosure of private repo content

## License

This document evolves with the project. See [LICENSE](LICENSE) for shithub's overall licensing terms.
