# gvault

`gvault` is a small CLI for encrypted file backups to a Git remote.

It is intended for simple personal backups run manually or from `cron`.

**note**: this is entirely vibecoded by codex.

## How It Works

`gvault` keeps two local repositories under `~/.gvault`:

- `mirror/`: a local plaintext Git repo used only to detect file changes
- `backups/`: the encrypted Git repo that is pushed to your configured remote

On `backup`, `gvault` mirrors your configured folders into `mirror/`, uses Git there to detect changed files, encrypts only those changed files into `backups/`, and pushes the encrypted repo.

If you later remove a folder from config, `gvault` stops tracking new changes for that folder but keeps its last backed-up state.

On `fetch`, `gvault` decrypts the encrypted repo contents into the destination you choose.

## Requirements

`gvault` shells out to:

- `git`

Build dependencies use the Go toolchain only.

## Install

```sh
go build .
```

Or install the binary with Go:

```sh
go install github.com/har777/gvault@latest
```

## Configuration

Run:

```sh
gvault init
```

This creates:

- `~/.gvault/config.json`
- `~/.gvault/logs.txt`
- `~/.gvault/mirror/` when backups start
- `~/.gvault/backups/` when backups start

The config fields are:

- `password`: used to encrypt each backed-up file
- `git_url`: remote Git repository for encrypted backups
- `folders`: comma-separated absolute paths to back up

Example:

```json
{
  "password": "your-password",
  "git_url": "git@github.com:you/private-backups.git",
  "folders": [
    "/Users/you/Documents/notes", 
    "/Users/you/Documents/journal"
  ]
}
```

## Usage

Create or update a backup:

```sh
gvault backup
```

If files changed, `gvault` prints:

```text
backup completed: N file(s) updated
```

If nothing changed, it prints:

```text
backup skipped: no changes
```

Restore the latest backup:

```sh
gvault fetch /path/to/restore
```

Restore a specific revision:

```sh
gvault fetch /path/to/restore <git-hash>
```

## Restore Layout

Each configured source folder is mirrored into a stable top-level directory name derived from its absolute path.

`gvault` makes the path filesystem-safe by replacing `/` with `__`.

Example source folders:

```text
/Users/you/code/notes
/Users/you/personal/notes
```

Restore layout:

```text
<destination>/Users__you__code__notes/...
<destination>/Users__you__personal__notes/...
```

`gvault` does not restore files back to their original absolute paths.

## Notes

- Source folder paths must be absolute
- The local `mirror/` repo is plaintext
- The remote backup repo stores encrypted file contents
- Symlinks are not supported

## Cron Example

```cron
30 2 * * * gvault backup >> /tmp/gvault-cron.log 2>&1
```
