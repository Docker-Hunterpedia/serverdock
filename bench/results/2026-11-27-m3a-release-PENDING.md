# M3a release bench: PENDING

This file is a placeholder. It holds no measurement. The maintainer replaces it with the results of the bench run for the M3a pull request (plan 0004 step 15) and copies the numbers into [docs/research/05-phase0-measurements.md](../../docs/research/05-phase0-measurements.md#m3a-release-measurements-step-15).

To produce them, from a clean checkout of the release commit (see `bench/run.sh` for every option):

- CI: the `bench` job on a pull request that is marked ready (it does not run on a draft). This gives the gated "S2 live watcher" and full S4 numbers.
- Locally, with a registry holding the test image on `127.0.0.1:5000`: `SCENARIOS="1 2 3 4 5 6 7 8" bench/run.sh` (add `RESULTS_SUFFIX=-final`; run a step 13 build with `BUILD_CONTEXT` and `RESULTS_SUFFIX=-step13` in the same session for the VmHWM comparison).
- The reload cases: `SD_TEST_RELOAD_CASES=1 SD_TEST_APP_IMAGE=127.0.0.1:5000/sd-testapp:1 go test -tags integration -run TestIntegrationReloadCasesWith100Routes -v -count=1 ./internal/deploy`, saving the `RESULT case=...` lines next to this file.

| Item | Status |
|---|---|
| CI "S2 live watcher" | PENDING (release gate; not run on a draft PR) |
| Full S4 | PENDING |
| S2 no Docker | PENDING |
| S2 with one exposed service | PENDING |
| S2 with 10 exposed idle services | PENDING |
| RSS 60 s after a deploy; peak during two concurrent deploys | PENDING |
| Binary, SPA JS | PENDING |
| Reload cases with 100 routes | PENDING |
