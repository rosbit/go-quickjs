/* The upstream build defines CONFIG_VERSION through a -D flag in its
 * Makefile; cgo cannot pass a quoted -D, so define it here, before the
 * QuickJS source is pulled in. Keep this in sync with the source version. */
#define CONFIG_VERSION "2026-06-04"
#include "csrc/quickjs.c"
