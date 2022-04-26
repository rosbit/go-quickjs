SHELL=/bin/bash

QUICKJS_VERSION = quickjs-2021-03-27
PKG = $(QUICKJS_VERSION).tar.xz
QUICKJS_SRC = https://bellard.org/quickjs/$(PKG)

LIB_QUICKJS = libquickjs.a

all: $(LIB_QUICKJS)

$(QUICKJS_VERSION):
	@echo "unpacking $(PKG) ..."
	tar Jxf $(PKG)

$(PKG): $(QUICKJS_VERSION)
	@if [ ! -e $(PKG) ]; then \
		@echo "downloading $@ ..."; \
		wget $(QUICKJS_SRC); \
	fi

$(LIB_QUICKJS): $(PKG)
	@echo "building $(QUICKJS_VERSION) ..."
	(cd $(QUICKJS_VERSION); make)
	cp -p $(QUICKJS_VERSION)/libquickjs.a .
