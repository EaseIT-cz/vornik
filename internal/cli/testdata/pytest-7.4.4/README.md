Output of `python -m pytest -q -p no:cacheprovider zz_acceptance_m_test.py`,
captured 2026-11-16 from pytest 7.4.4 inside the vornik-agent image (the bench
build of 7b5e44e3e), so the Python grader's markers are pinned against real
output (benchmark design, Amendment 2026-11-16, GitHub #650). Exit codes:
pass 0, fail 1, syntax 2, import 2; a hang under `timeout` exits 124.
