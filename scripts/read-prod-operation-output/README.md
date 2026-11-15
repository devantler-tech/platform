# Read a production operation's output

This repository's Actions logs are public. A dispatched production operation
(`.github/workflows/prod-operation.yaml`) therefore publishes only its exit
status and its stdout/stderr encrypted with age to the maintainer-held SOPS
recipient `age1rk6f…`. Anyone can see that a run happened and how it ended;
only the key holder can read what it printed.

To read a run:

```sh
go run ./scripts/read-prod-operation-output <run-id>
```

It fetches the run's job logs with your `gh` login, cuts out each encrypted
block and decrypts it locally with `age` (install it with `brew install age`).
The identity file is `AGE_IDENTITY_FILE`, then `SOPS_AGE_KEY_FILE`, then
`~/.config/sops/age/keys.txt`. The plaintext is printed only on your terminal.
