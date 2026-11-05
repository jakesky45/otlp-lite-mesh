.PHONY: all build test vet fmt tidy clean diagrams

# Diagrams are authored in draw.io (docs/diagrams/*.drawio) and exported to SVG with the
# source embedded, so the SVG can be re-opened in draw.io. Needs draw.io Desktop:
#   brew install --cask --appdir=$$HOME/Applications drawio
DRAWIO_SRC := $(wildcard docs/diagrams/*.drawio)
DRAWIO_SVG := $(DRAWIO_SRC:.drawio=.svg)

diagrams: $(DRAWIO_SVG)

docs/diagrams/%.svg: docs/diagrams/%.drawio
	drawio -x -f svg -e -b 10 -o $@ $<

all: vet test build

build:
	go build ./...

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

clean:
	rm -rf bin/

# Stage-specific targets (run-gw, run-edge, run-jaeger) are added as each stage lands.
