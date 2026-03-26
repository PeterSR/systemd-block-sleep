PREFIX := $(HOME)/bin

.PHONY: build install clean

build:
	go build -o block-sleep .

install: build
	mkdir -p $(PREFIX)
	cp block-sleep $(PREFIX)/

clean:
	rm -f block-sleep
