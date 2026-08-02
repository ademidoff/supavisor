#!/usr/bin/env bash
#
# Builds the published coverage site from a Go coverage profile.
#
#   coverage-site.sh <profile> <output-dir>
#
# Produces, in <output-dir>:
#   index.html     summary: total, per-file table, link to the annotated report
#   report.html    go tool cover -html output
#   coverage.json  shields.io endpoint descriptor for the README badge
#
# COMMIT and BUILT_AT may be set in the environment; both fall back to git.

set -euo pipefail

profile=${1:?usage: coverage-site.sh <profile> <output-dir>}
outdir=${2:?usage: coverage-site.sh <profile> <output-dir>}

module=$(awk '$1 == "module" { print $2; exit }' go.mod)
commit=${COMMIT:-$(git rev-parse HEAD)}
built_at=${BUILT_AT:-$(date -u +"%Y-%m-%d %H:%M UTC")}

mkdir -p "$outdir"

go tool cover -html="$profile" -o "$outdir/report.html"

# Per-file statement counts.
#
# With -coverpkg=./... every test binary reports on every package, so the same
# block appears once per binary. Key on the block to deduplicate, and sum the
# execution counts across binaries before deciding whether it was covered.
stats=$(awk '
	NR == 1 { next }
	{
		stmts[$1] = $2
		count[$1] += $3
	}
	END {
		for (key in stmts) {
			file = key
			sub(/:[0-9]+\.[0-9]+,[0-9]+\.[0-9]+$/, "", file)
			total[file] += stmts[key]
			grand_total += stmts[key]
			if (count[key] > 0) {
				covered[file] += stmts[key]
				grand_covered += stmts[key]
			}
		}
		for (file in total) {
			printf "file\t%s\t%d\t%d\t%.1f\n", file, covered[file] + 0, total[file], 100 * (covered[file] + 0) / total[file]
		}
		printf "total\t%d\t%d\t%.1f\n", grand_covered + 0, grand_total, 100 * grand_covered / grand_total
	}
' "$profile")

IFS=$'\t' read -r _ covered total pct <<<"$(grep '^total' <<<"$stats")"

# Same thresholds as the badge.
if awk "BEGIN { exit !($pct >= 80) }"; then
	status=good
	status_label="Good"
elif awk "BEGIN { exit !($pct >= 70) }"; then
	status=good
	status_label="Healthy"
elif awk "BEGIN { exit !($pct >= 50) }"; then
	status=warning
	status_label="Needs work"
else
	status=critical
	status_label="Low"
fi

case $status in
good) badge_color=$(awk "BEGIN { print ($pct >= 80) ? \"brightgreen\" : \"green\" }") ;;
warning) badge_color=yellow ;;
*) badge_color=red ;;
esac

cat >"$outdir/coverage.json" <<EOF
{
  "schemaVersion": 1,
  "label": "coverage",
  "message": "${pct}%",
  "color": "${badge_color}",
  "cacheSeconds": 300
}
EOF

rows=$(
	grep '^file' <<<"$stats" | sort -t"$(printf '\t')" -k5 -g | while IFS=$'\t' read -r _ file file_covered file_total file_pct; do
		path=${file#"$module/"}
		dir=$(dirname "$path")
		base=$(basename "$path")
		printf '<tr>'
		printf '<td class="path"><span class="dir">%s/</span><span class="base">%s</span></td>' "$dir" "$base"
		printf '<td class="num">%s</td><td class="num">%s</td>' "$file_total" "$file_covered"
		printf '<td class="meter-cell"><div class="meter"><div class="meter-fill" style="width:%s%%"></div></div></td>' "$file_pct"
		printf '<td class="num pct">%s%%</td>' "$file_pct"
		printf '</tr>\n'
	done
)

cat >"$outdir/index.html" <<EOF
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Supavisor test coverage</title>
<style>
:root {
	color-scheme: light;
	--surface: #fcfcfb;
	--page: #f9f9f7;
	--ink: #0b0b0b;
	--ink-secondary: #52514e;
	--ink-muted: #898781;
	--gridline: #e1e0d9;
	--border: rgba(11, 11, 11, 0.10);
	--meter-fill: #2a78d6;
	--meter-track: #cde2fb;
	--status-good: #0ca30c;
	--status-warning: #fab219;
	--status-critical: #d03b3b;
}

@media (prefers-color-scheme: dark) {
	:root {
		color-scheme: dark;
		--surface: #1a1a19;
		--page: #0d0d0d;
		--ink: #ffffff;
		--ink-secondary: #c3c2b7;
		--ink-muted: #898781;
		--gridline: #2c2c2a;
		--border: rgba(255, 255, 255, 0.10);
		--meter-fill: #3987e5;
		/*
		 * A wash of the fill hue rather than a darker step of the ramp: on the
		 * dark surface even the darkest documented blue still reads as a filled
		 * bar, so a low-coverage meter looked full.
		 */
		--meter-track: rgba(57, 135, 229, 0.24);
	}
}

* { box-sizing: border-box; }

body {
	margin: 0;
	padding: 48px 24px 72px;
	background: var(--page);
	color: var(--ink);
	font-family: system-ui, -apple-system, "Segoe UI", sans-serif;
	font-size: 14px;
	line-height: 1.5;
}

main {
	max-width: 860px;
	margin: 0 auto;
}

h1 {
	margin: 0;
	font-size: 15px;
	font-weight: 600;
	letter-spacing: 0.01em;
}

.subtitle {
	margin: 2px 0 0;
	color: var(--ink-secondary);
}

.card {
	margin-top: 24px;
	padding: 28px 28px 24px;
	background: var(--surface);
	border: 1px solid var(--border);
	border-radius: 10px;
}

.hero {
	font-size: 60px;
	line-height: 1;
	font-weight: 600;
	letter-spacing: -0.02em;
}

.hero-sub {
	margin-top: 10px;
	color: var(--ink-secondary);
}

.chip {
	display: inline-flex;
	align-items: center;
	gap: 7px;
	margin-top: 18px;
	padding: 4px 11px 4px 9px;
	border: 1px solid var(--border);
	border-radius: 999px;
	color: var(--ink-secondary);
	font-size: 13px;
}

.dot {
	width: 9px;
	height: 9px;
	border-radius: 50%;
	background: var(--status-${status});
}

.actions {
	margin-top: 24px;
	padding-top: 20px;
	border-top: 1px solid var(--gridline);
}

a.button {
	color: var(--meter-fill);
	font-weight: 600;
	text-decoration: none;
}

a.button:hover { text-decoration: underline; }

h2 {
	margin: 40px 0 0;
	font-size: 13px;
	font-weight: 600;
	text-transform: uppercase;
	letter-spacing: 0.06em;
	color: var(--ink-secondary);
}

.hint {
	margin: 4px 0 0;
	color: var(--ink-muted);
	font-size: 13px;
}

table {
	width: 100%;
	margin-top: 14px;
	border-collapse: collapse;
}

th {
	padding: 0 12px 8px;
	border-bottom: 1px solid var(--gridline);
	color: var(--ink-muted);
	font-size: 12px;
	font-weight: 600;
	text-align: right;
	text-transform: uppercase;
	letter-spacing: 0.05em;
	white-space: nowrap;
}

th:first-child { padding-left: 0; text-align: left; }
th.meter-col { width: 140px; text-align: left; }

td {
	padding: 9px 12px;
	border-bottom: 1px solid var(--gridline);
	vertical-align: middle;
}

tr:last-child td { border-bottom: none; }
td:first-child { padding-left: 0; }

.path {
	font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
	font-size: 13px;
	white-space: nowrap;
}

.dir { color: var(--ink-muted); }
.base { color: var(--ink); }

.num {
	color: var(--ink-secondary);
	font-variant-numeric: tabular-nums;
	text-align: right;
	white-space: nowrap;
}

.pct { color: var(--ink); font-weight: 600; }

.meter {
	width: 140px;
	height: 8px;
	background: var(--meter-track);
	border-radius: 4px;
	overflow: hidden;
}

.meter-fill {
	height: 100%;
	background: var(--meter-fill);
	border-radius: 0 4px 4px 0;
}

footer {
	max-width: 860px;
	margin: 28px auto 0;
	color: var(--ink-muted);
	font-size: 12px;
}

footer a { color: inherit; }
</style>
</head>
<body>
<main>
	<h1>Supavisor</h1>
	<p class="subtitle">Test coverage, measured with the race detector on every push to main.</p>

	<div class="card">
		<div class="hero">${pct}%</div>
		<div class="hero-sub">${covered} of ${total} statements covered</div>
		<div class="chip"><span class="dot"></span>${status_label}</div>
		<div class="actions">
			<a class="button" href="report.html">View annotated source &rarr;</a>
		</div>
	</div>

	<h2>By file</h2>
	<p class="hint">Least covered first. Statement counts show how much code each percentage stands for.</p>
	<table>
		<thead>
			<tr>
				<th>File</th>
				<th>Statements</th>
				<th>Covered</th>
				<th class="meter-col">Coverage</th>
				<th></th>
			</tr>
		</thead>
		<tbody>
${rows}
		</tbody>
	</table>
</main>
<footer>
	Generated ${built_at} from
	<a href="https://github.com/ademidoff/supavisor/commit/${commit}">${commit:0:7}</a>.
	Packages with no statements executed by any test do not appear.
</footer>
</body>
</html>
EOF

echo "Total coverage: ${pct}% (${covered}/${total} statements)"
