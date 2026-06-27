# cside/wtgquery/Makefile — libwtgquery.a + sample 빌드.
#
# PoC — mds query-server W9501S01 (종가 조회) 의 WTG mci-chart 백엔드 wrapper.
# 외부 의존 0 — POSIX socket / sys lib 만 사용. cside/wtgpush, cside/wtgprice
# 와 동일 원칙. mymq 의 멀티 플랫폼 (AIX/Solaris/HPUX/Linux/Darwin) 에서
# 그대로 빌드.
#
# 통합:
#   make                # libwtgquery.a + sample 빌드
#   make sample         # sample 만
#   make clean
#
# NH 사내 client link:
#   gcc -I.../cside/wtgquery my_query.c -L.../cside/wtgquery -lwtgquery -o my_query

CC ?= cc
AR ?= ar
CFLAGS ?= -O -g -Wall -Wextra
LDLIBS_PLATFORM =

UNAME := $(shell uname)
ifeq ($(UNAME),SunOS)
LDLIBS_PLATFORM = -lsocket -lnsl
endif

LIB     = libwtgquery.a
OBJ     = wtgquery.o
SAMPLE  = sample

.PHONY: all clean

all: $(LIB) $(SAMPLE)

$(LIB): $(OBJ)
	$(AR) rcs $@ $^

wtgquery.o: wtgquery.c wtgquery.h
	$(CC) $(CFLAGS) -c -o $@ wtgquery.c

$(SAMPLE): sample.c $(LIB)
	$(CC) $(CFLAGS) -o $@ sample.c $(LIB) $(LDLIBS_PLATFORM)

clean:
	rm -f $(OBJ) $(LIB) $(SAMPLE)
