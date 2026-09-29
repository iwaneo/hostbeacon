#!/bin/sh
# Agent budget check (v1 spec §4.6, §14): measures the installed, running
# Agent on this Host with its default intervals. Passes when both parts
# together use at most 30 MB of memory and 1% of one CPU core on average.
#
# Memory is the resident memory of the two programs, sampled every 10
# seconds; the highest sample counts. CPU is what the two systemd units used,
# with the programs they start (apt, dnf, smartctl), over the whole run.
# Run it on real systemd, not in QEMU.
# Usage: budget-check.sh [seconds, default 3600]
set -eu
duration=${1:-3600}
units="hostbeacon.service hostbeacon-helper.service"
limit_kb=$((30 * 1024))
limit_cpu=1

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

pids() {
	for unit in $units; do
		pid=$(systemctl show --property MainPID --value "$unit")
		[ "$pid" -gt 0 ] || fail "$unit is not running"
		printf '%s ' "$pid"
	done
}

# CPU time of both units in microseconds, from their cgroups.
cpu_usec() {
	total=0
	for unit in $units; do
		file=/sys/fs/cgroup$(systemctl show --property ControlGroup --value "$unit")/cpu.stat
		[ -r "$file" ] || fail "cannot read $file (needs cgroup v2)"
		total=$((total + $(awk '$1 == "usage_usec" { print $2 }' "$file")))
	done
	echo "$total"
}

rss_kb() {
	total=0
	for pid in $1; do
		total=$((total + $(awk '$1 == "VmRSS:" { print $2 }' "/proc/$pid/status")))
	done
	echo "$total"
}

start_pids=$(pids)
start_cpu=$(cpu_usec)
start=$(date +%s)
end=$((start + duration))
max_kb=0
sum_kb=0
samples=0
while :; do
	[ "$(pids)" = "$start_pids" ] || fail "the Agent restarted during the check"
	kb=$(rss_kb "$start_pids")
	[ "$kb" -gt "$max_kb" ] && max_kb=$kb
	sum_kb=$((sum_kb + kb))
	samples=$((samples + 1))
	now=$(date +%s)
	[ "$now" -lt "$end" ] || break
	left=$((end - now))
	sleep $((left < 10 ? left : 10))
done
cpu=$(($(cpu_usec) - start_cpu))
elapsed=$(($(date +%s) - start))

. /etc/os-release
echo "Host: $(hostname) ($PRETTY_NAME, $(uname -m)), $(hostbeacon version)"
awk -v max="$max_kb" -v sum="$sum_kb" -v n="$samples" -v cpu="$cpu" -v s="$elapsed" \
	-v limit_kb="$limit_kb" -v limit_cpu="$limit_cpu" 'BEGIN {
	percent = cpu / (s * 1e6) * 100
	printf "Measured for %d s\n", s
	printf "Memory, both parts: highest %.1f MB, average %.1f MB (limit %d MB)\n", max / 1024, sum / n / 1024, limit_kb / 1024
	printf "CPU, both parts: %.3f%% of one core on average (limit %d%%)\n", percent, limit_cpu
	if (max > limit_kb) { print "FAIL: memory is over the limit"; exit 1 }
	if (percent > limit_cpu) { print "FAIL: CPU is over the limit"; exit 1 }
	print "PASS"
}'
