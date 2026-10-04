.PHONY: deployment-smoke burst readiness-check verify-clean-clone

export BASE_URL
export JWT_SECRET

# Run a live end-to-end API smoke test. Pass BASE_URL and JWT_SECRET on the
# command line; credentials are not stored in this Makefile or the test script.
deployment-smoke:
	@python3 scripts/deployment_smoke.py

burst:
	@python3 scripts/burst.py $(if $(OUT),--out "$(OUT)")

readiness-check:
	@python3 scripts/readiness_check.py
