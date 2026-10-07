cmake_minimum_required(VERSION 3.16)
project(avbench CXX)
set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()
# -ffp-contract=off keeps floating point results identical between compilers
# that would otherwise fuse multiply-adds differently, which the checksum
# would read as a different output.
add_compile_options(-Wall -Wextra -Werror -ffp-contract=off)
add_executable(avbench avbench.cpp)
option(AVBENCH_STATIC "link statically, for rigs that should not depend on the host's libstdc++" OFF)
if(AVBENCH_STATIC)
  target_link_options(avbench PRIVATE -static)
endif()
