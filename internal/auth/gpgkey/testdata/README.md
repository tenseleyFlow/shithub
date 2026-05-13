# gpgkey testdata

This directory exists for future committed fixtures (real-world
`gpg`-produced ASCII-armored blocks that might be useful as
regression-test inputs). It is **empty by default**.

The current `parse_test.go` synthesizes its fixtures in-memory via
`github.com/ProtonMail/go-crypto/openpgp` so:

- Tests run without `gpg` installed (CI portability).
- Fixtures are deterministic (no time-bomb expiry races).
- The `private` and `signature` armor-block fixtures don't have to
  be committed as files (they're constructed on demand from synthesized
  entities).

If a future bug surfaces from a specific real-world key shape, drop the
producing key here as `<shape>.asc` and reference it from
`parse_test.go` via `os.ReadFile`. Keep keys throwaway; never commit
material from a real user.
