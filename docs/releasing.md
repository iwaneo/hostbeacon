# Releasing

A release is the release gate of the v1 spec (§4.6, §14). Every step must pass.

1. **CI passes on `main`**, including the load test: 100 simulated Agents
   against a test Home Assistant; the Integration's added CPU stays under 5%
   of one core.
2. **Tag** `v<major>.<minor>.<patch>` on `main` and push it. The release
   workflow builds the packages and runs the Agent budget check for 1 hour on
   a native arm64 runner. The owner then approves the `release` environment,
   and the workflow signs and publishes the release with the arm64 numbers in
   its notes.
3. **Agent budget on the test Hosts**: install the release on the Debian and
   Fedora test Hosts, then run
   `sudo sh agent/packaging/test/budget-check.sh` on each (1 hour). Limits:
   at most 30 MB memory and 1% of one core on average, both parts together.
   Add the numbers to the release notes. A QEMU-emulated run does not count.
4. **Manual checks** on both test Hosts with the test Home Assistant (real
   systemd), and check the Home Assistant screens:
   - Install: the `.deb` or `.rpm` from the release, then `hostbeacon setup`
   - Pair: add the Host by address with a Pairing code
   - Sensors: every capability of the Host shows a value
   - Update run: from the Updates entity, to the end
   - Reboot: from the Reboot button; Host status goes Rebooting, then Online
   - Agent update: from the previous release, through Home Assistant
   - Re-pair: from Reconfigure with a new Pairing code
   - Clone paths: a changed machine ID (the Agent becomes a copy, then
     Re-pair), identity hold and `keep-identity`, and `reset-identity`
5. **HACS**: the release installs as a custom repository.
