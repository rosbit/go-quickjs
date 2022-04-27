SHELL=/bin/bash

QUICKJS_VERSION = quickjs-2021-03-27
PKG = $(QUICKJS_VERSION).tar.xz
QUICKJS_SRC = https://bellard.org/quickjs/$(PKG)

LIB_QUICKJS = libquickjs.a

all: $(LIB_QUICKJS)

$(QUICKJS_VERSION): $(PKG)
	@echo "unpacking $(PKG) ..."
	tar Jxf $(PKG)

$(PKG):
	@echo "downloading $@ ..."
	wget $(QUICKJS_SRC)

$(LIB_QUICKJS): $(QUICKJS_VERSION)
	@echo "building $(QUICKJS_VERSION) ..."
	(cd $(QUICKJS_VERSION); make)
	cp -p $(QUICKJS_VERSION)/$(LIB_QUICKJS) .
