<!-- What this change does and why, in a sentence or two. -->

## Checks

Run these before asking for review; CI runs the same set.

- [ ] `go vet ./...`
- [ ] `go test ./...`
- [ ] `gofmt -l .` prints nothing
- [ ] `gitleaks dir . --redact`
- [ ] `claude plugin validate .`

## Nothing personal, ever

- [ ] This change adds nothing about a specific person: no names, handles, emails, hostnames, account IDs, ARNs, internal URLs, or employers.
      Examples and tests use placeholders like `you`, `example.com`, and `laptop-a`.
