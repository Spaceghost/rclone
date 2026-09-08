# Read-only backend integration

From the repository root:

```console
RCLONE_CONFIG=/notfound GOFLAGS=-mod=readonly go run ./fstest/test_all \
  -config backend/backup/testdata/integration.yaml \
  -backends backup -maxtries 1 -n 1
```

This profile needs no cloud account, existing vault, saved rclone configuration,
Kopia executable, or service. The Go tests create encrypted Kopia repositories
in temporary directories and open them through the registered backup backend.
No backup credentials or test repositories are retained in the report.

`TestIntegration` restores complete directory trees from `latest`, an explicit
snapshot, an explicitly incomplete snapshot, and a rooted subdirectory. It checks
file bytes, modification times, Unicode names, empty files and directories,
ranges and EOF, and permission errors for all six required mutation methods.
A final SHA-256 inventory requires every repository file and its contents to
remain unchanged, including after reader shutdown. Other backend tests retain
coverage of pinned catalogues, ambiguous sources, deduplication and stock-Kopia
interoperability.

`TestIntegrationProfile` requires one actual backend job with no ignored failures
or automatic retries. The existing backup workflow runs the tests on Linux,
macOS and Windows; its Linux standalone job also runs this profile and retains
the test_all reports as a source-associated artifact.

The generic `fstests.Run` fixture creates directories and uploads objects before
most read tests. It is not a read-only test harness. This profile deliberately
uses the test_all driver with backend-specific fixtures, not writable-destination
`fs/operations`, `fs/sync`, bisync or VFS suites, and does not claim those suites
passed. Recursive restores exercise the production `sync.CopyDir` implementation
with backup as the source and a disposable local filesystem as the destination.

These checks qualify local filesystem-layout repositories only. Remote storage,
physical power loss, full restore metadata fidelity, public backup creation,
retention and upstream design approval remain separate gates.
