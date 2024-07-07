set(CMAKE_MODULE_PATH ${CMAKE_MODULE_PATH} "${CMAKE_CURRENT_SOURCE_DIR}/cmake")

cmake_minimum_required(VERSION 3.20)

project(WebRTCSFU Go)

include(cmake/golang.cmake)
include(cmake/flags.cmake)

add_subdirectory(sfu)
add_subdirectory(peer)

