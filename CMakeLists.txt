set(CMAKE_MODULE_PATH ${CMAKE_MODULE_PATH} "${CMAKE_CURRENT_SOURCE_DIR}/cmake")

cmake_minimum_required(VERSION 3.20)

project(WebRTCSFU Go)

include(cmake/golang.cmake)
include(cmake/flags.cmake)

if(APPLE)
    # go compiler does not like to build universal binaries.
    # so for use on the Mac we always build Intel
    set(CMAKE_GO_GOARCH "amd64")
endif()

add_subdirectory(sfu)
add_subdirectory(peer)

