# cside/wtgprice/Makefile — libwtgprice.a + sample 빌드.
#
# 외부 의존 0 — POSIX socket / sys lib 만 사용. cside/wtgpush 와 동일 원칙.
# mymq 의 멀티 플랫폼 (AIX/Solaris/HPUX/Linux/Darwin) 에서 그대로 빌드.
#
# 통합:
#   make                # libwtgprice.a + sample 빌드
#   make sample         # sample 만
#   make clean
#
# 매칭 엔진 link:
#   gcc -I.../cside/wtgprice my_match.c -L.../cside/wtgprice -lwtgprice -o my_match

CC ?= cc
AR ?= ar
CFLAGS ?= -O -g -Wall -Wextra
LDLIBS_PLATFORM =

UNAME := $(shell uname)
ifeq ($(UNAME),SunOS)
LDLIBS_PLATFORM = -lsocket -lnsl
endif

LIB     = libwtgprice.a
OBJ     = wtgprice.o
SAMPLE  = sample

.PHONY: all clean

all: $(LIB) $(SAMPLE)

$(LIB): $(OBJ)
	$(AR) rcs $@ $^

wtgprice.o: wtgprice.c wtgprice.h
	$(CC) $(CFLAGS) -c -o $@ wtgprice.c

$(SAMPLE): sample.c $(LIB)
	$(CC) $(CFLAGS) -o $@ sample.c $(LIB) $(LDLIBS_PLATFORM)

clean:
	rm -f $(OBJ) $(LIB) $(SAMPLE)
