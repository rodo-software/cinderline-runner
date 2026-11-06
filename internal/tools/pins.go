package tools

// The pinned agent CLIs and their hashes (EMBER-57), the same versions as
// today's runner image. Bump them on purpose, all in this file: a new set
// gets a new volume.
const (
	ClaudeVersion = "2.1.316"
	// OpenCodeVersion is the version of the npm platform packages
	// opencode-linux-<arch>[-musl].
	OpenCodeVersion = "1.19.5"
	PonytailRepo    = "https://github.com/DietrichGebert/ponytail"
	PonytailTag     = "v5.3.2"
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

// muslLibsImage supplies libstdc++ and libgcc_s, which OpenCode's musl build
// needs and a bare Alpine image lacks. Pinned by digest, so Docker checks
// it by hash (node 22.23.4 on Alpine 3.24.4, multi-arch index).
const muslLibsImage = "node:22-alpine@sha256:0e1754758c2df4cfa3e68d59e2e1b7374bb7fd9ea6328eaaacf15cc934af8202"
