# Introduction

This is a small debug program to illustrate the issue depicted in [this PR](https://github.com/danielpaulus/go-ios/pull/462).

The idea is to run multiple concurrent calls to `NewWithAddrPort` to illustrate that this use-case basically ends up with a deadlock 100% of the time. Whereas the same use-case protected against concurrent calls succeeds 100% of the time.

# Requirements

- Linux
- go > 1.20.0
- usbmuxd
- an iPhone (preferrably running iOS >= 17)

# Usage

1. Build the program :
    ```
    go build
    ```
2. Run the program once without concurrency safety :
    ```
    sudo ./go-ios-debug
    ```
    And notice that it gets stuck after a while.
3. Run the program another time, this time with concurrency safety :
    ```
    sudo ./go-ios-debug -s
    ```
    And notice that it succeeds.

    _Note : You might have to kill the previous run using `kill -9 <pid>` as it should be stuck and prevent another program from using port `60106`_