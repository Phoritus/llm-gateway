# 20 Optional: stop the first-run stall on new Go binaries

Status: ready-for-human

Freshly built test binaries stall for 10 seconds to 13 minutes on their first
socket use, because of security scanning. Once it pushed `make check` past
Go's 10-minute test timeout. Excluding Go's build cache (`go env GOCACHE`) from
Windows Security removes it. It is a security setting, so it is the user's
call.

## Comments
