@echo off
rem Builds standalone cub2tif executables into dist\ (needs Go: https://go.dev/dl/)
setlocal
set CGO_ENABLED=0
set FLAGS=-trimpath -ldflags "-s -w"
set PKG=.\cmd\cub2tif
if not exist dist mkdir dist
go vet ./... || exit /b 1
go test ./... || exit /b 1
set GOOS=windows& set GOARCH=amd64& go build %FLAGS% -o dist\cub2tif.exe %PKG% || exit /b 1
set GOOS=linux& set GOARCH=amd64& go build %FLAGS% -o dist\cub2tif-linux-amd64 %PKG% || exit /b 1
set GOOS=darwin& set GOARCH=arm64& go build %FLAGS% -o dist\cub2tif-macos-arm64 %PKG% || exit /b 1
set GOOS=darwin& set GOARCH=amd64& go build %FLAGS% -o dist\cub2tif-macos-amd64 %PKG% || exit /b 1
echo Built:
dir /b dist
