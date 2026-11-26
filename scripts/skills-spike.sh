#!/bin/sh
# The checks from docs/skills/client-loading.md that need a real model and
# real keys (CLR-256). Each check is one headless agent turn in a container,
# with the package mounted the way that document recommends. A check passes
# when the output shows the model used the skill or saw the package's context.
#
#   IMAGE=<agent image> scripts/skills-spike.sh all
#   IMAGE=<agent image> scripts/skills-spike.sh claude-skill
#
# Needs docker, a glibc IMAGE (on musl, Gemini and OpenCode also need the
# volume's libraries) and the tools volume a runner has filled. Each check
# needs its client's key in the environment: ANTHROPIC_API_KEY, CODEX_API_KEY
# (an OpenAI key), GEMINI_API_KEY, or OPENROUTER_API_KEY plus OPENCODE_MODEL
# (e.g. openrouter/<model id>). PKG is a package checkout on this machine;
# by default, ponytail is copied from the tools volume. Outputs stay in OUT.
set -eu

N=cinderline
T=/opt/$N
: "${IMAGE:?set IMAGE to the agent image}"
TOOLS=${TOOLS:-$(docker volume ls -q --filter "name=$N-tools-" | head -n 1)}
[ -n "$TOOLS" ] || { echo "no $N-tools-* volume: let a runner fill it first" >&2; exit 1; }
OUT=${OUT:-$PWD/skills-spike-out}
mkdir -p "$OUT/repo"
chmod 777 "$OUT/repo"
[ -d "$OUT/repo/.git" ] || git -C "$OUT/repo" init -q
if [ -z "${PKG:-}" ]; then
	PKG=$OUT/ponytail
	[ -d "$PKG" ] || docker run --rm --user "$(id -u):$(id -g)" -v "$TOOLS:/t:ro" -v "$OUT:/o" --entrypoint cp "$IMAGE" -r /t/ponytail /o/
fi

# run is one agent container as the runner starts it: user 1000, the tools
# volume read-only, a fresh state folder, the repository as working folder.
run() {
	docker run --rm -i --user 1000:1000 --tmpfs /state:uid=1000,gid=1000,exec \
		-v "$TOOLS:$T:ro" -v "$OUT/repo:/work" -w /work -e DISABLE_AUTOUPDATER=1 "$@"
}

# check runs one check and greps its output for what proves it.
check() {
	name=$1 want=$2
	shift 2
	if "$@" >"$OUT/$name.out" 2>&1 && grep -q -- "$want" "$OUT/$name.out"; then
		echo "PASS $name"
	else
		echo "FAIL $name: see $OUT/$name.out"
	fi
}

claude() {
	printf '%s' "$1" | run -e ANTHROPIC_API_KEY -e CLAUDE_CONFIG_DIR=/state/claude -e CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
		-e CLAUDE_CODE_DISABLE_CLAUDE_MDS=1 -v "$PKG:/skills/pkg:ro" "$IMAGE" \
		$T/bin/claude -p --max-turns 5 --output-format stream-json --verbose --dangerously-skip-permissions \
		--strict-mcp-config --plugin-dir /skills/pkg
}

codex() {
	printf '%s' "$1" | run -e CODEX_API_KEY -e CODEX_HOME=/state/codex -v "$PKG:/state/codex/skills/pkg:ro" "$IMAGE" \
		sh -c 'mkdir -p "$CODEX_HOME" && export PATH="'$T'/rg:$PATH" && exec "$0" "$@"' \
		$T/bin/codex exec --json --dangerously-bypass-approvals-and-sandbox \
		-c features.plugins=false -c features.api_key_model_discovery=false -c project_doc_max_bytes=0 -
}

# gemini takes the prompt and the docker mount flags for the package.
gemini() {
	p=$1
	shift
	printf '%s' "$p" | run -e GEMINI_API_KEY -e GEMINI_CLI_HOME=/state/gemini \
		-e SETTINGS='{"security":{"auth":{"selectedType":"gemini-api-key"}},"privacy":{"usageStatisticsEnabled":false}}' \
		"$@" "$IMAGE" \
		sh -c 'mkdir -p "$GEMINI_CLI_HOME/.gemini" && printf %s "$SETTINGS" > "$GEMINI_CLI_HOME/.gemini/settings.json" && exec "$0" "$@"' \
		$T/bin/node $T/gemini/bundle/gemini.js -p "" --output-format stream-json --yolo --skip-trust
}

# opencode takes the prompt and the extra inline config members.
opencode() {
	[ -n "${OPENCODE_MODEL:-}" ] || { echo "set OPENCODE_MODEL, e.g. openrouter/<model id>"; return 1; }
	cfg='{"autoupdate":false,"provider":{"openrouter":{"options":{"apiKey":"{env:OPENROUTER_API_KEY}"}}},"agent":{"build":{"steps":5}},'$2'}'
	printf '%s' "$1" | run -e OPENROUTER_API_KEY -e XDG_DATA_HOME=/state/opencode/data -e XDG_CONFIG_HOME=/state/opencode/config \
		-e OPENCODE_DISABLE_MODELS_FETCH=1 -e OPENCODE_CONFIG_CONTENT="$cfg" -v "$PKG:/skills/pkg:ro" "$IMAGE" \
		sh -c 'export PATH="'$T'/rg:$PATH" && exec "$0" "$@"' \
		$T/bin/opencode run --format json --auto --title spike --model "$OPENCODE_MODEL"
}

use_skill='Use the ponytail-help skill, then reply with the first heading in it.'
sees() { echo "Does your context contain the exact text \"$1\"? Reply YES or NO only."; }

for c in claude-skill claude-hook codex-skill gemini-skill gemini-extension opencode-skill opencode-plugin; do
	[ "${1:-all}" = all ] || [ "$1" = "$c" ] || continue
	case $c in
	claude-skill) check $c '"name":"Skill"' claude "$use_skill" ;;
	# The SessionStart hook runs node: it needs node on the image's PATH.
	claude-hook) check $c '"result":"YES' claude "$(sees 'PONYTAIL MODE ACTIVE')" ;;
	codex-skill) check $c 'ponytail-help/SKILL.md' codex "$use_skill" ;;
	gemini-skill) check $c '"tool_name":"activate_skill"' gemini "$use_skill" \
		-v "$PKG/skills/ponytail-help:/state/gemini/.agents/skills/ponytail-help:ro" ;;
	gemini-extension) check $c '"content":"YES' gemini "$(sees 'You are a lazy senior developer')" \
		-v "$PKG:/state/gemini/.gemini/extensions/pkg:ro" ;;
	opencode-skill) check $c '"tool":"skill"' opencode "$use_skill" '"skills":{"paths":["/skills/pkg/skills"]}' ;;
	opencode-plugin) check $c '"text":"YES' opencode "$(sees 'PONYTAIL MODE')" \
		'"plugin":["/skills/pkg/.opencode/plugins/ponytail.mjs"]' ;;
	esac
done
