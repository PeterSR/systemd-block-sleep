PREFIX := $(HOME)/bin

.PHONY: build install clean

build:
	go build -o block-sleep .

install: build
	mkdir -p $(PREFIX)
	install -m755 block-sleep $(PREFIX)/
	systemctl --user try-restart block-sleep-when.service 2>/dev/null || true

clean:
	rm -f block-sleep
