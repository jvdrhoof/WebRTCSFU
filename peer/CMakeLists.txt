set(SOURCE_FILE main.go log.go packet.go priority_lock.go proxy_connection.go signaling.go transcoder.go )
add_go_executable(WebRTCSFU-peer ${SOURCE_FILE})
