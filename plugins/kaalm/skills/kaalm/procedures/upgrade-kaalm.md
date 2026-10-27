# Upgrade Kaalm

Use this to move an installed Kaalm to the version this skill matches (the line "This skill matches Kaalm v..." in [SKILL.md](../SKILL.md)). Write it without the `v` as `VERSION`. To move to a different version, the user first installs the skill for that version, as the version check in SKILL.md says. The page behind this procedure is [Upgrading Kaalm](https://github.com/win07xp/kaalm/blob/main/guide/src/getting-started/upgrading.md).

Running agents are not restarted by an upgrade. The gateway rollout resets its idle tracking, so idle and hibernation changes wait for one idle timeout afterward. Tell the user this before you start.

## Within 2.x

1. **Check the starting point.** Run the version check in [SKILL.md](../SKILL.md#the-version-check). If the installed version is 2.x and older than `VERSION`, continue. If it is 1.x or 0.x, stop: [Moving from 1.x or 0.x](https://github.com/win07xp/kaalm/blob/main/guide/src/getting-started/upgrading.md#moving-from-1x-or-0x) removes every Kaalm resource and the data on agent volumes. Take the user through it only one step at a time, with an OK for each, and never skip its step that sets `pvcRetention`.
2. **Apply the new CRDs first.** Helm never upgrades CRDs. A *CRD* is the definition of one of Kaalm's kinds of object. The commands from [Apply the new chart's CRDs](https://github.com/win07xp/kaalm/blob/main/guide/src/getting-started/upgrading.md#1-apply-the-new-charts-crds):

   ```bash
   helm pull oci://ghcr.io/win07xp/charts/kaalm --version VERSION --untar --untardir SCRATCH_DIR
   kubectl --context CONTEXT apply --dry-run=server --server-side --force-conflicts -f SCRATCH_DIR/kaalm/crds/
   ```

   Explain the change, ask for the OK, and run the second command again without `--dry-run=server`. `--force-conflicts` is part of the command, as the page explains. Replace `SCRATCH_DIR` with a new empty folder outside the user's project; `helm pull` unpacks the chart there.
3. **Read the current Helm values.** `helm upgrade` resets any value you leave out, so pass the install's values again:

   ```bash
   helm --kube-context CONTEXT get values kaalm -n NAMESPACE -o yaml
   ```

   The chart takes names of Secrets, not their contents, so this output holds no keys. Save it to a file in `SCRATCH_DIR`; this is `VALUES_FILE` below.
4. **Upgrade the release** through [change-safely.md](change-safely.md). [Upgrade the release](https://github.com/win07xp/kaalm/blob/main/guide/src/getting-started/upgrading.md#2-upgrade-the-release) has the command:

   ```bash
   helm --kube-context CONTEXT upgrade kaalm oci://ghcr.io/win07xp/charts/kaalm \
     --version VERSION --namespace NAMESPACE \
     -f VALUES_FILE --wait
   ```
5. **Verify** with the checks in [Verifying the install](https://github.com/win07xp/kaalm/blob/main/guide/src/getting-started/verifying.md), and check that agents and tasks read as they did before:

   ```bash
   kubectl --context CONTEXT get agents,agenttasks -A
   ```
6. **Save the version:** `python3 SKILL_DIR/scripts/config.py set install.chartVersion VERSION`.
