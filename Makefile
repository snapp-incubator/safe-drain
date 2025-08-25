.PHONY: tidy vendor

tidy:
	go mod tidy

vendor: tidy
	go mod vendor
