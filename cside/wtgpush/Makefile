# cside/wtgpush/Makefile — libwtgpush.a + sample 빌드.
#
# 외부 의존 0 — POSIX socket / sys lib 만 사용. mymq 의 멀티 플랫폼 환경
# (AIX/Solaris/HPUX/Linux/Darwin) 에서 그대로 빌드.
#
# 통합:
#   make                # libwtgpush.a + sample 빌드
#   make sample         # sample 만
#   make clean
#
# mymq AP 에서 link:
#   gcc -I.../cside/wtgpush my_ap.c -L.../cside/wtgpush -lwtgpush -o my_ap

CC ?= cc
AR ?= ar
CFLAGS ?= -O -g -Wall -Wextra
LDLIBS_PLATFORM =

# 플랫폼별 추가 라이브러리 — Solaris 는 socket / nsl 명시.
UNAME := $(shell uname)
ifeq ($(UNAME),SunOS)
LDLIBS_PLATFORM = -lsocket -lnsl
endif

LIB     = libwtgpush.a
OBJ     = wtgpush.o
SAMPLE  = sample

.PHONY: all clean

all: $(LIB) $(SAMPLE)

$(LIB): $(OBJ)
	$(AR) rcs $@ $^

wtgpush.o: wtgpush.c wtgpush.h
	$(CC) $(CFLAGS) -c -o $@ wtgpush.c

$(SAMPLE): sample.c $(LIB)
	$(CC) $(CFLAGS) -o $@ sample.c $(LIB) $(LDLIBS_PLATFORM)

clean:
	rm -f $(OBJ) $(LIB) $(SAMPLE)
