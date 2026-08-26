# Contributing to pylon-beacon

Thanks for being here. pylon-beacon is a small, friendly project, and pull
requests, bug reports, questions, and "here's a weird box I couldn't monitor"
stories are all welcome. You do not need to be a Go expert or ask permission
before opening something — a rough patch with a clear description beats a
perfect one that never gets sent.

## The shape of the thing

The whole agent is standard-library Go across about a dozen files, roughly
3,400 lines. There are no third-party dependencies and there never will be:
this binary runs on machines people care about, and "read it before you trust
it" only means something if reading it is actually feasible. If a change would
pull in a dependency, that is a conversation to have first, not a surprise in a
diff.

A quick map so you know where to look:

- `main.go` — config parsing, the push loop, and the HTTPS POST.
- `collect_linux.go` / `collect_windows.go` / `collect_other.go` — the built-in
  vitals, one file per platform behind build tags.
- `probes.go` — the `[probes]` LAN checks (http / tcp / ping).
- `snmp.go` — the `[snmp]` reader (v2c, GET-only).
- `proxmox.go` — the `[proxmox]` integration via `pvesh`.
- `logwatch.go` — `[logwatch]`: tail a file, count regex matches in a window.
- `logship.go` — `[logs]`: ship the lines themselves.
- `*_test.go` — the tests, next to what they test.

## Building and testing

You need Go 1.22 or newer. Nothing else.

```sh
git clone https://github.com/PylonMon/pylon-beacon
cd pylon-beacon
go build -o pylon-beacon .
go test ./...
go vet ./...
```

`go build` cross-compiles cleanly — if you touch a platform collector you
cannot run, build it for that platform to be sure it still compiles:

```sh
GOOS=windows go build ./...
GOOS=linux   go build ./...
```

Please run `gofmt` (or `go fmt ./...`) before you push. A CI run that fails on
formatting is a slow way to learn you missed it.

## A few principles the code already follows

These are not rules handed down; they are patterns that came from running this
on real machines, and keeping to them keeps the agent trustworthy. If your
change fits them it will land faster, and if you think one of them is wrong,
say so — they have been wrong before.

- **The silence is the signal.** The beacon's job is to push, so paging always
  outranks anything else it does. A slow SNMP device, a dark LAN probe, a log
  flood — none of them may delay or block a check-in. Bound the work by the
  push cycle; drop rather than stall.
- **No data is not zero.** A metric that stops arriving must not read as `0`,
  and a healthy counter is reported as `0` rather than omitted. "The device is
  gone" and "the device says zero" are different facts and must look different.
  This is why several integrations report an explicit `*_up` metric.
- **Nothing listens.** The agent makes outbound requests and opens no ports.
  Any feature that would require an inbound connection, a tunnel, or a
  listening socket is the wrong shape for this project.
- **First sight starts at the end.** A newly-installed watch (logs, logwatch)
  begins at the end of the file. Installing the beacon must never page someone
  about last week's errors.
- **Degrade honestly.** When a command, poll, or CLI fails, report *nothing*
  for that metric and keep the heartbeat going. Never invent a value to fill a
  gap.

## Opening a pull request

- Small and focused is easier to review than large and sweeping. If you have
  two ideas, two PRs.
- Say what problem it solves and how you tested it — "ran it on a Pi 4 and a
  UniFi UDM for a day" is genuinely useful.
- Add or update a test when the change is testable. The existing tests are a
  fair guide to the style; unit tests that avoid real network/hardware are
  preferred, and anything that needs live gear is guarded so `go test ./...`
  stays green for everyone.
- New config keys or metrics should get a line in the README so people can
  actually find them.

## Reporting bugs and asking for things

Open an issue. Helpful things to include, when you have them: the OS and
version, the relevant slice of `beacon.conf` (with your key redacted), and what
you saw versus what you expected. For "it can't monitor my X" — tell us what X
is; a lot of this agent exists because someone had a box nobody else supported.

## A note on tone and attribution

Commit messages and everything committed here should read as written by a
person, in plain language. Skip AI/tooling attribution and co-author trailers
in this repo — not because there is anything to hide, but because this is a
public project for a skeptical audience and the writing should stand on its own.

## License

By contributing, you agree that your contributions are licensed under the
project's [MIT license](LICENSE).
