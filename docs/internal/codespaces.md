# Codespaces status

shithub does not currently implement a Codespaces-equivalent product.
This document records the SP28 stopline decision so billing copy,
navigation, and future implementation work do not confuse Actions
runners with hosted development environments.

## Current state

The S41 Actions campaign shipped runner infrastructure:

- isolated runner hosts;
- per-job workspaces;
- Docker-based execution;
- logs, artifacts, checks, and policy controls.

That substrate is useful, but it is not Codespaces. Actions jobs are
short-lived CI executions. Codespaces require persistent, user-facing
development sessions with interactive access and a lifecycle the user
controls.

The top-level `/codespaces` route intentionally renders an unavailable
page instead of a 404. This keeps the global navigation honest while
making the gap explicit.

## Paid-org contract

GitHub advertises Codespaces as part of its organization plan
comparison. shithub must not mark that row as `Included` until a real
hosted development environment product exists.

The current plan comparison row is:

- Free: `Not available`
- Team: `Not available`
- Enterprise: `Contact sales`
- State: `Launch blocker`

SP32 must treat this as a paid-organization GA blocker unless the
owner explicitly changes the parity bar.

## What a real implementation needs

A future Codespaces campaign must ship all of these before pricing copy
can describe Codespaces as available:

- workspace/session tables with explicit state transitions;
- repository and organization policy for eligible repos;
- persistent workspace storage with hard limits;
- authenticated browser or SSH access;
- idle timeout and retention cleanup;
- abuse controls before starting compute;
- usage metering separate from Actions minutes;
- billing separate from Team licensed seats.

The private planning file `.docs/sprints/S50-codespaces-campaign.md`
owns the real implementation backlog.
