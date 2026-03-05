BINARY  := fleet
CMD     := ./cmd/fleet

.PHONY: build test lint vet check clean clean-test-repos

build:
	go build -o $(BINARY) $(CMD)

test:
	go test -short ./...

vet:
	go vet ./...

lint: vet
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./...; fi

check: lint test build

clean:
	rm -f $(BINARY)

# Delete all fleet-test-* repos created by integration tests on GitHub.
# Requires: gh auth refresh -s delete_repo (one-time)
clean-test-repos:
	@echo "Searching for fleet-test-* repos..."
	@repos=$$(gh repo list --json name,owner -q '.[] | select(.name | startswith("fleet-test-")) | .owner.login + "/" + .name' 2>/dev/null); \
	if [ -z "$$repos" ]; then \
		echo "  No fleet-test-* repos found."; \
	else \
		echo "  Found:"; \
		echo "$$repos" | sed 's/^/    /'; \
		echo ""; \
		read -p "  Delete all? [y/N]: " ans; \
		if [ "$$ans" = "y" ] || [ "$$ans" = "Y" ]; then \
			echo "$$repos" | while read -r r; do \
				echo "  deleting $$r ..."; \
				gh repo delete "$$r" --yes 2>&1 | sed 's/^/    /'; \
			done; \
			echo "  Done."; \
		else \
			echo "  Cancelled."; \
		fi; \
	fi
