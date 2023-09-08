SFU_SRC=sfu/main.go
PEER_SRC=peer/main.go peer/packet.go peer/proxy_connection.go peer/signaling.go peer/transcoder.go
UNAME=$(shell uname)
ifeq ($(UNAME), Darwin)
ALL=sfu_mac.exe peer_mac.exe
endif
ifeq ($(UNAME), Linux)
ALL=sfu_linux.exe peer_linux.exe
endif
# Need something for Windows, if we have Make there.

all: $(ALL)
	echo Built all for $(UNAME)

sfu_mac.exe: $(SFU_SRC)
	go build -o sfu_mac.exe ./sfu
	
peer_mac.exe: $(PEER_SRC)
	go build -o peer_mac.exe ./peer