native:
	CGO_ENABLED=0 go build -o udpfec-go -trimpath -ldflags "-s -w -buildid=" .

.PHONY: native
