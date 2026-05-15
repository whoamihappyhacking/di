PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin

.PHONY: build install clean

build:
	go build -o di .

install: build
	./di install

clean:
	rm -f di
