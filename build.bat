@echo off
setlocal EnableDelayedExpansion

:: WinClaw build script
:: Requires Go 1.21+ installed and in PATH
:: Usage: build.bat [release]

echo WinClaw Build Script
echo.

:: Check Go is available
where go >nul 2>&1
if errorlevel 1 (
    echo ERROR: Go not found in PATH.
    echo Download Go 1.21+ from https://go.dev/dl/
    exit /b 1
)

for /f "tokens=3" %%v in ('go version') do set GO_VERSION=%%v
echo Go: %GO_VERSION%
echo.

:: Download / tidy dependencies
echo Fetching dependencies...
go mod tidy
if errorlevel 1 (
    echo ERROR: go mod tidy failed.
    exit /b 1
)
echo Dependencies OK.
echo.

:: Vet
echo Running go vet...
go vet ./...
if errorlevel 1 (
    echo ERROR: go vet reported issues.
    exit /b 1
)
echo Vet OK.
echo.

:: Build flags
set LDFLAGS=-s -w -X main.version=v0.1.0
set BUILD_TAGS=windows

if "%1"=="release" (
    echo Building release binary...
    go build -tags %BUILD_TAGS% -ldflags "%LDFLAGS%" -o winclaw.exe ./cmd/winclaw
) else (
    echo Building debug binary...
    go build -tags %BUILD_TAGS% -o winclaw.exe ./cmd/winclaw
)

if errorlevel 1 (
    echo ERROR: Build failed.
    exit /b 1
)

echo.
echo Build successful: winclaw.exe
echo.
echo First-time setup:
echo   winclaw.exe --setup
echo.
echo Start WinClaw:
echo   winclaw.exe

endlocal
