# WebRTCSFU

> This section is outdated. And mostly untrue...

In this repository, all files related to a multi-party volumetric video-based system are made available. Four parts are considered:

- A Unity project written in C#; based on the [VR2Gather project](https://github.com/cwi-dis/VR2Gather) developed at DIS/CWI, Amsterdam, The Netherlands
- A plugin written in C++, used to deal with incoming/outgoing video frames
- A WebRTC sender/receiver written in Golang, used to communicate with other parties
- A WebRTC selective forwarding unit (SFU) written in Golang, used to interconnect peers

The system is currently under development by IDLab, Ghent University - imec. This README will be updated while development continues, with detailed instructions for each of these components.

## Build instructions

Building is done with `cmake`. It should work on all major platforms.

```
cmake -S . -B build -DCMAKE_INSTALL_PREFIX=./installed
cmake --build build
cmake --build build --target install
```

This will create two executables in `installed/bin`:
- `WebRTCSFU-sfu` is the SFU, which can be used with the VR2Gather orchestrator
- `WebRTCSFU-peer` is the client process, which can be used the VR2Gather Unity applications through the `WebRTCConnector` DLL.

Cmake is probably overkill for golang, but it makes it easier to use github actions to do automatic build and creating of releases, and tha in turn makes it easier to integrate these into VR2Gather.