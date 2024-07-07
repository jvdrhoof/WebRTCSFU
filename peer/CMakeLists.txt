set(SOURCE_FILE main.go packet.go priority_lock.go transcoder.go packet.go signaling.go)
add_go_executable(WebRTCSFU-peer ${SOURCE_FILE})
