package tools

// The pinned agent CLIs and their hashes (EMBER-57), the same versions as
// today's runner image. Bump them on purpose, all in this file: a new set
// gets a new volume.
const (
	ClaudeVersion = "2.1.316"
	// OpenCodeVersion is the version of the npm platform packages
	// opencode-linux-<arch>[-musl].
	OpenCodeVersion = "1.19.5"
	// CodexVersion is the GitHub release rust-v<CodexVersion> of openai/codex.
	CodexVersion = "0.167.0"
	PonytailRepo = "https://github.com/DietrichGebert/ponytail"
	PonytailTag  = "v5.3.2"
	// PonytailCommit is what PonytailTag points at; the clone is refused
	// otherwise, so a moved tag can't change the plugin.
	PonytailCommit = "16352ac229eb08a9598dcae48fe9f2b342567d86"
)

// claudeSHA256 is per platform, copied from
// downloads.claude.ai/claude-code-releases/<ClaudeVersion>/manifest.json.
var claudeSHA256 = map[string]string{
	"linux-x64":        "fd13c70000fec3f2116dfbcbb6121210b4a29328bf0e46210a59dd594a67d824",
	"linux-arm64":      "55cdb8e162d190432696ea54010e0fca2e18d06fa7a7127fd563b9ffd4f22624",
	"linux-x64-musl":   "8bc3d015479b8b87dfde3f69f9bdb96c658758a8eb31892fd8fbece8d8a08bcf",
	"linux-arm64-musl": "d774cb39055cff62af476af32702f1fc4f888a7e395a37434a95d6f5a98818a0",
}

// openCodeSRI is the npm integrity of each opencode-<platform> tarball, from
// registry.npmjs.org/opencode-<platform>/<OpenCodeVersion>.
var openCodeSRI = map[string]string{
	"linux-x64":        "sha512-i1qh7VooLGML9xX6ud93y0JA8j/1aPeDYRKjsmYXrJX1TjIWHgBBMkEbEJPZthpWbsJg//XfBgR6Kb7+NPaJiQ==",
	"linux-arm64":      "sha512-u2HFRI6v1nNMVKiV34An8XrGBxJeZ+RCg2tYu3SqlGODKqKsr5RoEbaX1tnsfGb+rETh9AJVX9kS8dSZXPufsA==",
	"linux-x64-musl":   "sha512-y6nUbH3ViajN37KaVVy0/KnWv/i48Bx5QPv9Ul9ONJya1ossV7CHVb+ZNFc61M5GqlVtjCvZV7T0eBMqhJG7mg==",
	"linux-arm64-musl": "sha512-YxvpVPJ/jeevV93DVRiuOVbhkoJAuwQUadPDHPaw65aZkAKZ6OE7XySwCS+Rjlhzt7fPryisvAOjUtZkwfMBuQ==",
}

// codexSHA256 is per arch, the SHA-256 of codex-<target>.tar.gz as the GitHub
// release API lists it (assets[].digest) for rust-v<CodexVersion>. Codex ships
// static musl builds only, which run on glibc and musl images alike.
var codexSHA256 = map[string]string{
	"amd64": "9d64836106d5b374e90ee4454c7ebcde7ca7106558bd7e41a34b9f26ebda251a",
	"arm64": "843489acb4d4b1079745c36b03c5963b887b83155af208fd53584e33ee8eb281",
}

// codexHostSHA256 is per arch, the SHA-256 of codex-code-mode-host-<target>.tar.gz
// from the same release; Codex runs code mode in this host, which it looks
// for next to its own binary.
var codexHostSHA256 = map[string]string{
	"amd64": "b5074d5dd64643b2c073927e494a588eff31c1f5a27f12c1e2c45f9a6cf07dd4",
	"arm64": "6e99948fa20ca816a01f222a56afa261b321e00cbed611d76bb92c765fd11ec6",
}

// muslLibsImage supplies libstdc++ and libgcc_s, which OpenCode's musl build
// needs and a bare Alpine image lacks. Pinned by digest, so Docker checks
// it by hash (node 22.23.4 on Alpine 3.24.4, multi-arch index).
const muslLibsImage = "node:22-alpine@sha256:0e1754758c2df4cfa3e68d59e2e1b7374bb7fd9ea6328eaaacf15cc934af8202"
