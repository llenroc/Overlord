# Copyright 2015 The Chromium OS Authors. All rights reserved.
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

GO=go
BINDIR=$(CURDIR)/bin
BUILD=$(CURDIR)/build
DEPS?=true
STATIC?=false
LDFLAGS=

ifeq ($(STATIC), true)
	LDFLAGS=-a -tags netgo -installsuffix netgo
endif

all: ghost overlordd

deps:
	mkdir -p $(BINDIR)
	if $(DEPS); then \
		cd $(CURDIR)/overlord; \
		$(GO) get -d .; \
	fi

overlordd: deps
	cd $(CURDIR)/cmd/$@ && GOBIN=$(BINDIR) $(GO) install $(LDFLAGS) .
	rm -f $(BINDIR)/app
	ln -s $(CURDIR)/overlord/app $(BINDIR)

ghost: deps
	cd $(CURDIR)/cmd/$@ && GOBIN=$(BINDIR) $(GO) install $(LDFLAGS) .

py-bin:
	mkdir -p $(BUILD)
	# Create virtualenv environment
	rm -rf $(BUILD)/.env
	virtualenv $(BUILD)/.env
	# Build ovl binary with pyinstaller
	cd $(BUILD); \
	. $(BUILD)/.env/bin/activate; \
	pip install jsonrpclib ws4py pyyaml pyinstaller; \
	pyinstaller --onefile $(CURDIR)/scripts/ovl.py; \
	pyinstaller --onefile $(CURDIR)/scripts/ghost.py
	# Move built binary to bin
	mv $(BUILD)/dist/ovl $(BINDIR)/ovl.py.bin
	mv $(BUILD)/dist/ghost $(BINDIR)/ghost.py.bin

clean:
	rm -rf $(BINDIR)/ghost $(BINDIR)/overlordd $(BUILD) \
		$(BINDIR)/ghost.py.bin $(BINDIR)/ovl.py.bin
