# git-fat

git-fat keeps large binary files out of a git repository while they still
show up, complete, in the working tree. Git stores a 74-byte stub for each fat
file. The content goes to `.git/fat/objects` on your machine and to an S3
bucket (or any S3-compatible store) for everyone else.

Committing binaries directly makes every clone carry every version of every
binary forever, and binary diffs compress badly. With git-fat, clones stay small
and fast, history and `git bisect` still work, and each person pulls only the
fat files they need.

git-fat is a git clean/smudge filter. After `git fat init`, `git add`,
`git checkout` and friends handle fat files without extra steps. You run
`git fat push` and `git fat pull` to move content to and from the store.

## Install

Each `v*` tag publishes these assets plus `SHA256SUMS`:

- `git-fat-<version>-linux-x86_64.tar.gz`, `git-fat-<version>-linux-aarch64.tar.gz`
- `git-fat-<version>-darwin-x86_64.tar.gz`, `git-fat-<version>-darwin-aarch64.tar.gz`
- `git-fat-<version>-windows-x86_64.zip`
- `git-fat-<version>-x86_64.msi`, a Windows installer that puts git-fat on
  `PATH`
- `git-fat-<version>-universal.pkg`, a macOS installer with Intel and Apple
  silicon binaries

The binary has to be on `PATH` as `git-fat` for `git fat` to work.

With [mise](https://mise.jdx.dev), on any of the platforms above:

```sh
mise use -g github:hurricanehrndz/git-fat
```

On macOS, the pkg installs `/usr/local/bin/git-fat`. The pkg and the darwin
binaries are signed with a Developer ID and notarized by Apple. Install it from
Installer.app or a shell:

```sh
sudo installer -pkg git-fat-<version>-universal.pkg -target /
```

On Windows without mise, run the MSI. It installs `C:\Program Files\git-fat`
and appends that directory to the system `PATH`, so open a new terminal
afterwards. Uninstalling removes it from `PATH` again. The MSI is not signed,
so SmartScreen may warn before it runs. Silent install:
`msiexec /i git-fat-<version>-x86_64.msi /qn`.

With Go 1.26.9 or newer:

```sh
go install github.com/hurricanehrndz/git-fat/v2/cmd/git-fat@latest
```

## Set up a repository

Mark the fat files in `.gitattributes`:

```
*.png filter=fat -text
*.zip filter=fat -text
```

Describe the store in `.gitfat` at the top of the repository:

```toml
[s3]
bucket = "s3://my-fat-store"
```

Commit both files so every clone uses the same settings, then run once per
clone:

```sh
git fat init
```

`init` sets `filter.fat.clean`, `filter.fat.smudge`, `filter.fat.process` and
`filter.fat.required` in the repository's local config. Any git-fat command
runs it for you if the filter is not configured yet.

From then on, `git add` stores fat files in `.git/fat/objects` and stages their
stubs. Run `git fat push` before or after `git push` so others can fetch the
content.

## .gitfat

`.gitfat` is TOML. The first table names the store type and must be `[s3]`.
Keys are case-sensitive, and an unknown key is an error.

```toml
[s3]
bucket = "s3://my-fat-store"   # required; the s3:// prefix is optional
prefix = "fat/"                # optional key prefix; a trailing / is added
region = "us-west-2"           # optional, see below
endpoint = "https://s3.example.com"  # optional S3-compatible endpoint (path-style)
id = "AKIA..."                 # optional static credentials, used only
secret = "..."                 # when both id and secret are set

# Applied to every upload. Names and meanings are boto3's ExtraArgs:
# ACL, CacheControl, ChecksumAlgorithm, ContentDisposition, ContentEncoding,
# ContentLanguage, ContentType, ExpectedBucketOwner, Expires,
# GrantFullControl, GrantRead, GrantReadACP, GrantWriteACP, Metadata,
# ObjectLockLegalHoldStatus, ObjectLockMode, ObjectLockRetainUntilDate,
# RequestPayer, ServerSideEncryption, StorageClass, SSECustomerAlgorithm,
# SSECustomerKey, SSECustomerKeyMD5, SSEKMSKeyId, SSEKMSEncryptionContext,
# Tagging, WebsiteRedirectLocation.
[s3.xpushargs]
ACL = "bucket-owner-full-control"
Metadata = { team = "games" }
Expires = 2030-01-01T00:00:00Z  # datetimes need a UTC offset

# Optional second store for `git fat publish`. It takes the same keys
# as [s3] except smudgestore.
[s3.smudgestore]
bucket = "s3://my-published-files"
[s3.smudgestore.xpushargs]
ACL = "public-read"
```

Objects in the fat store are keyed by their SHA1. Files published to the
smudge store are keyed by their path in the repository, so other systems can
fetch them by name.

**Credentials.** Without `id` and `secret`, git-fat uses the AWS SDK's default
chain: environment variables, shared config and credentials files (profiles
and SSO), then instance metadata.

**Region.** `region` in `.gitfat` always wins. Without it, git-fat asks S3 for
the bucket's region with one anonymous request, and uses the region from the
AWS chain (`AWS_REGION`, the profile) only if that request fails. The chain's
region would break every store outside it, because S3 rejects requests signed
for the wrong region. With `endpoint` set, git-fat uses the chain's region, or
`us-east-1`.

TLS certificates are verified. For a store with a private CA, set
`AWS_CA_BUNDLE` to the CA's PEM file.

## Commands

```
git fat init                     configure the fat filters for this repo
git fat push                     upload the index's fat objects the store lacks
git fat pull (--all | files...)  download fat objects and restore their files
git fat pull --base <ref>        pull fat files HEAD adds or changes vs ref
git fat check [files...]         check the index's fat objects are in the store
git fat check --base <ref>       check fat files staged as added or changed vs ref are in the store
git fat publish --base <ref>     upload fat files HEAD adds or changes vs ref to the smudgestore
git fat version                  print the version
```

`--base` limits a command to fat files added or changed relative to a ref.
`pull` and `publish` compare HEAD with it, `check` compares the index, which
is what a pre-commit hook needs. Deleted files are skipped.

`filter-clean`, `filter-smudge` and `filter-process` are what git calls; you do
not run them yourself.

`pull` only replaces a working file that still holds its stub, so local edits
are never overwritten. Every download is checked against the stub's SHA1 and
size before it enters `.git/fat/objects`. A clone that has no fat objects yet
shows stubs in place of fat files until you pull.

`GIT_FAT_VERBOSE=1` prints informational messages as well as warnings.
`DRYRUN=1` makes uploads (`push`, `publish`) print what they would upload
instead.

Fat objects live under git's common directory, so linked worktrees share one
`fat/objects`.

## pre-commit hooks

This repository provides two [pre-commit](https://pre-commit.com) hooks.
pre-commit builds git-fat from source with Go (`language: golang`).

```yaml
repos:
  - repo: https://github.com/hurricanehrndz/git-fat
    rev: v2.1.0
    hooks:
      - id: gitfat-check-new   # pre-commit: staged fat objects new vs master are in the store
        # args: [main]           # compare against another ref
      - id: gitfat-check       # pre-merge-commit: fat objects of the files pre-commit passes are in the store
```

## Upgrading from 1.x (Python)

2.x reads the same `.gitfat` and writes byte-identical stubs, so existing
repositories and stores work unchanged. Uninstall the `yelp-gitfat` Python
package (its `yelp-gitfat` alias is gone) and install git-fat as above. Then:

- Re-run `git fat init` in each existing clone. It adds `filter.fat.process`,
  which runs one git-fat process per git command instead of one per file, and
  `filter.fat.required`, which makes `git add` fail when the clean filter fails
  instead of committing the full file.
- TLS certificates are verified. 1.x disabled verification. See
  `AWS_CA_BUNDLE` above for private CAs.
- Unknown or wrongly cased keys in `[s3]`, `[s3.smudgestore]` and their
  `xpushargs` are errors. 1.x ignored them. The error names the right spelling
  when only the case differs.
- The first table in `.gitfat` must be `[s3]`. 1.x used the first table,
  whatever its name, as S3 settings.
- With no region configured, 1.x relied on boto3 following S3's redirects;
  2.x looks up the bucket's region instead (see Region).
- Fat objects are stored in `<git common dir>/fat/objects`. This is the same
  `.git/fat/objects` in a normal clone, and makes linked worktrees work.
- Smudging content that is not a stub passes it through. 1.x wrote nothing,
  which emptied the checked-out file.
- A stub is exactly 74 bytes. A file that merely starts with a stub is
  treated as content.
- The commands are renamed. The 1.x names still work, unlisted:

  | 1.x                   | 2.x                       |
  |-----------------------|---------------------------|
  | `fscheck [files...]`  | `check [files...]`        |
  | `fscheck-new [ref]`   | `check --base <ref>`      |
  | `pull-new [ref]`      | `pull --base <ref>`       |
  | `fspublish-new [ref]` | `publish --base <ref>`    |

  The 1.x names still default to `master` when given no ref. `--base` has no
  default.
- `--base` also catches fat files that changed, not only those added.
  `check --base` checks the index (what is about to be committed) instead of
  the working tree.
- `pull` restores any working file that still holds its stub, even when the
  object was already cached, and leaves files with local changes alone. It
  verifies each download's SHA1 and size and keeps the working file's
  permission bits (1.x made restored files read-only). As before, the file
  gets the cached object's modification time.
- `publish` no longer restores working files on the way.
- `push` warns about fat objects that are neither cached locally nor in the
  store, since nobody can fetch them.

The rsync store and the retroactive import (`git fat find`,
`git fat index-filter`) from the original git-fat are not supported in 2.x.

## Build

```sh
go build ./cmd/git-fat   # or: just build
just test                # unit and end-to-end tests
just integration         # adds the S3 tests; needs rustfs on PATH
just lint
```

`nix develop` (or direnv) gives a shell with Go, golangci-lint, just and
rustfs. Releases bump the version with `just bump [patch|minor|major]`.
