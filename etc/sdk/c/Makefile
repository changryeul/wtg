# WTG QuoteID C SDK — 빌드.
#
# 의존:
#   - libcurl   (apt: libcurl4-openssl-dev / brew: curl / RHEL: libcurl-devel)
#   - cJSON     (apt: libcjson-dev         / brew: cjson / RHEL: cjson-devel)
#
# 사용:
#   make            — quoteid_client.o + example_fix_flow 빌드
#   make lib        — 정적 라이브러리 libquoteid.a
#   make clean

CC      ?= cc
CFLAGS  ?= -O2 -Wall -Wextra -std=c99 -fPIC
CFLAGS  += $(shell pkg-config --cflags libcurl libcjson 2>/dev/null)
LDLIBS  ?= $(shell pkg-config --libs   libcurl libcjson 2>/dev/null)
ifeq ($(strip $(LDLIBS)),)
LDLIBS  := -lcurl -lcjson
endif

OBJS    := quoteid_client.o
TARGETS := example_fix_flow

.PHONY: all lib clean

all: $(OBJS) $(TARGETS)

quoteid_client.o: quoteid_client.c quoteid_client.h
	$(CC) $(CFLAGS) -c quoteid_client.c -o $@

example_fix_flow: example_fix_flow.c quoteid_client.o
	$(CC) $(CFLAGS) example_fix_flow.c quoteid_client.o $(LDLIBS) -o $@

lib: libquoteid.a

libquoteid.a: $(OBJS)
	ar rcs $@ $^

clean:
	rm -f $(OBJS) $(TARGETS) libquoteid.a
