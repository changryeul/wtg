# sise read-shim — 벤더 sise 대신 mci-price-krx SHM 을 읽는 최소 클라이언트 lib.
# trn/mon 이 -lmfsise -lmfnsise -lmbsise -lcomsise 로 링크 (drop-in). 헤더는 vendored.
INC ?= ../krxshm/inc
CFLAGS = -I$(INC) -O2 -Wall -D_GNU_SOURCE
LIBS = libmfsise.a libmfnsise.a libmbsise.a libcomsise.a

all: $(LIBS)
lib%.a: %_shim.o
	ar rcs $@ $<
%_shim.o: %_shim.c
	cc $(CFLAGS) -c $< -o $@
test: $(LIBS) test_shim.c
	cc $(CFLAGS) test_shim.c -o test_shim -L. -lmfsise -lmbsise -lmfnsise -lcomsise -lrt
clean:
	rm -f *.o *.a test_shim
