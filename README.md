In this repository, all files related to a multi-party volumetric video-based system are made available. Four parts are considered:

- A Unity project in C#; based on the VR2Gather project of DIS/CWI, Amsterdam, The Netherlands
- A plugin in C++, used to deal with incoming/outgoing video frames
- A WebRTC sender/receiver in Golang, used to communicate with other parties
- A WebRTC selective forwarding unit (SFU) in Golang, used to interconnect peers

The system is currently under development by IDLab, Ghent University - imec. This README will be updated while development continues, with detailed instructions for each of these components.
