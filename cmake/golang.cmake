set(GOPATH "${CMAKE_CURRENT_BINARY_DIR}/go")
file(MAKE_DIRECTORY ${GOPATH})

function(ExternalGoProject_Add TARG)
  add_custom_target(${TARG} ${CMAKE_COMMAND} -E env GOPATH=${GOPATH} ${CMAKE_Go_COMPILER} get ${ARGN})
endfunction(ExternalGoProject_Add)

function(add_go_executable NAME)
  set(SUFFIXEDNAME "${NAME}${CMAKE_EXECUTABLE_SUFFIX}")
  file(GLOB GO_SOURCE RELATIVE "${CMAKE_CURRENT_SOURCE_DIR}" "*.go")
  set(optEnvArch "")
  if(CMAKE_GO_GOARCH)
    set(optEnvArch "GOARCH=${CMAKE_GO_GOARCH}")
  endif()
  set(optEnvOs "")
  if(CMAKE_GO_GOOS)
    set(optEnvOs "GOOS=${CMAKE_GO_GOOS}")
  endif()
  add_custom_command(
    OUTPUT ${OUTPUT_DIR}/.timestamp 
    COMMAND ${CMAKE_COMMAND} -E env GOPATH=${GOPATH} ${optEnvArch} ${optEnvOs} ${CMAKE_Go_COMPILER} build -o "${CMAKE_CURRENT_BINARY_DIR}/${SUFFIXEDNAME}" ${CMAKE_GO_FLAGS} ${GO_SOURCE}
    WORKING_DIRECTORY ${CMAKE_CURRENT_LIST_DIR}
    )

  add_custom_target(${NAME} ALL DEPENDS ${OUTPUT_DIR}/.timestamp ${ARGN})
  install(PROGRAMS ${CMAKE_CURRENT_BINARY_DIR}/${SUFFIXEDNAME} DESTINATION bin)
endfunction(add_go_executable)

# Note by Jack: this doesn't necessarily install to the right place.
function(ADD_GO_LIBRARY NAME BUILD_TYPE)
  if(BUILD_TYPE STREQUAL "STATIC")
    set(BUILD_MODE -buildmode=c-archive)
    set(LIB_NAME "lib${NAME}.a")
  else()
    set(BUILD_MODE -buildmode=c-shared)
    if(APPLE)
      set(LIB_NAME "lib${NAME}.dylib")
    elif(WIN32)
      set(LIB_NAME "${NAME}.dll")
    else()
      set(LIB_NAME "lib${NAME}.so")
    endif()
  endif()

  file(GLOB GO_SOURCE RELATIVE "${CMAKE_CURRENT_SOURCE_DIR}" "*.go")
  add_custom_command(
    OUTPUT ${OUTPUT_DIR}/.timestamp
    COMMAND ${CMAKE_COMMAND} -E env GOPATH=${GOPATH} ${CMAKE_Go_COMPILER} build ${BUILD_MODE} -o "${CMAKE_CURRENT_BINARY_DIR}/${LIB_NAME}" ${CMAKE_GO_FLAGS} ${GO_SOURCE}
    WORKING_DIRECTORY ${CMAKE_CURRENT_LIST_DIR}
    )

  add_custom_target(${NAME} ALL DEPENDS ${OUTPUT_DIR}/.timestamp ${ARGN})

  if(NOT BUILD_TYPE STREQUAL "STATIC")
    install(PROGRAMS ${CMAKE_CURRENT_BINARY_DIR}/${LIB_NAME} DESTINATION bin)
  endif()
endfunction(ADD_GO_LIBRARY)
