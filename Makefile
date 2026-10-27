# Build:   make              -> ./mpts2spts (static binary)
# Release: make dist         -> dist/*.deb and dist/*.tar.gz
#          make dist VERSION=1.2.0
VERSION ?= $(shell git describe --tags --abbrev=0 2>/dev/null | sed 's/^v//' || true)
ifeq ($(VERSION),)
VERSION := 1.0.0
endif
ARCH    := amd64
NAME    := mpts2spts
PKG     := $(NAME)_$(VERSION)_$(ARCH)
DIST    := dist

GOFLAGS := -trimpath -tags 'osusergo netgo sqlite_omit_load_extension'
LDFLAGS := -s -w -X main.version=$(VERSION) -linkmode external -extldflags '-static'

.PHONY: all build deb tar dist clean
all: build

build:
	CGO_ENABLED=1 GOARCH=$(ARCH) go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(NAME) .

deb: build
	rm -rf $(DIST)/deb && mkdir -p $(DIST)/deb/DEBIAN
	install -D -m 0755 $(NAME) $(DIST)/deb/opt/$(NAME)/$(NAME)
	install -D -m 0644 packaging/mpts2spts.env $(DIST)/deb/opt/$(NAME)/mpts2spts.env.example
	install -D -m 0644 packaging/setup-common.sh $(DIST)/deb/opt/$(NAME)/setup-common.sh
	install -D -m 0644 mpts2spts.service $(DIST)/deb/lib/systemd/system/mpts2spts.service
	install -m 0755 packaging/debian/postinst packaging/debian/prerm packaging/debian/postrm $(DIST)/deb/DEBIAN/
	printf '%s\n' \
	  'Package: $(NAME)' \
	  'Version: $(VERSION)' \
	  'Architecture: $(ARCH)' \
	  'Maintainer: Super Media Tech <https://www.supermediatech.com>' \
	  'Section: video' \
	  'Priority: optional' \
	  'Depends: systemd, passwd' \
	  'Installed-Size: '"$$(du -sk $(DIST)/deb --exclude=DEBIAN | cut -f1)" \
	  'Homepage: https://www.supermediatech.com' \
	  'Description: Low-CPU MPTS to SPTS splitter with web UI' \
	  ' Splits multicast MPTS inputs into single-program SPTS outputs (UDP/RTP),' \
	  ' managed from a web interface. Configuration is stored in SQLite.' \
	  > $(DIST)/deb/DEBIAN/control
	fakeroot dpkg-deb --build -Zxz $(DIST)/deb $(DIST)/$(PKG).deb
	rm -rf $(DIST)/deb

tar: build
	rm -rf $(DIST)/$(PKG) && mkdir -p $(DIST)/$(PKG)
	cp $(NAME) mpts2spts.service README.md packaging/install.sh packaging/uninstall.sh packaging/setup-common.sh $(DIST)/$(PKG)/
	cp packaging/mpts2spts.env $(DIST)/$(PKG)/mpts2spts.env.example
	tar -C $(DIST) -czf $(DIST)/$(PKG).tar.gz $(PKG)
	rm -rf $(DIST)/$(PKG)

dist: deb tar
	cd $(DIST) && sha256sum $(PKG).deb $(PKG).tar.gz > $(PKG).sha256
	@ls -la $(DIST)

clean:
	rm -rf $(NAME) $(DIST)
