#!/usr/bin/env bash
# Build the Android APK on a Linux provider machine.
# JDK 17 and the Android SDK are provisioned into ./out when missing, so a fresh
# machine only needs curl + unzip + network. Gradle comes from the Tencent mirror
# configured in gradle/wrapper/gradle-wrapper.properties.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
if [ -x "$ROOT/gradlew" ]; then
  PROJ="$ROOT"
elif [ -x "$ROOT/android/gradlew" ]; then
  PROJ="$ROOT/android"
else
  echo "[ERROR] gradlew not found next to $0"
  exit 1
fi
OUT="$ROOT/out"
mkdir -p "$OUT"

# ---- JDK 17 ----
if [ -z "${JAVA_HOME:-}" ] || [ ! -x "$JAVA_HOME/bin/javac" ]; then
  JAVA_HOME=""
  for d in /usr/lib/jvm/*17* /usr/lib/jvm/*-17-* /opt/jdk-17* "$HOME/.sdkman/candidates/java/17"*; do
    if [ -x "$d/bin/javac" ]; then JAVA_HOME="$d"; break; fi
  done
fi
if [ -z "${JAVA_HOME:-}" ]; then
  echo "[provision] downloading Temurin JDK 17"
  mkdir -p "$OUT/jdk"
  curl -fsSL --retry 3 -o "$OUT/jdk.tar.gz" \
    "https://api.adoptium.net/v3/binary/latest/17/ga/linux/x64/jdk/hotspot/normal/eclipse"
  tar -xzf "$OUT/jdk.tar.gz" -C "$OUT/jdk" --strip-components=1
  JAVA_HOME="$OUT/jdk"
fi
export JAVA_HOME
export PATH="$JAVA_HOME/bin:$PATH"
echo "[build] JAVA_HOME=$JAVA_HOME"

# ---- Android SDK ----
SDK="${ANDROID_HOME:-$HOME/Android/Sdk}"
export ANDROID_HOME="$SDK"
export ANDROID_SDK_ROOT="$SDK"
mkdir -p "$SDK"

SDKM="$SDK/cmdline-tools/latest/bin/sdkmanager"
if [ ! -x "$SDKM" ]; then
  echo "[provision] downloading Android cmdline-tools"
  curl -fsSL --retry 3 -o "$OUT/cmdline-tools.zip" \
    "https://dl.google.com/android/repository/commandlinetools-linux-11076708_latest.zip"
  rm -rf "$OUT/ct" "$SDK/cmdline-tools/latest"
  mkdir -p "$OUT/ct" "$SDK/cmdline-tools"
  unzip -q "$OUT/cmdline-tools.zip" -d "$OUT/ct"
  mv "$OUT/ct/cmdline-tools" "$SDK/cmdline-tools/latest"
fi

if [ ! -d "$SDK/platforms/android-34" ]; then
  echo "[provision] accepting licenses and installing SDK packages"
  yes | "$SDKM" --sdk_root="$SDK" --licenses >/dev/null 2>&1 || true
  yes | "$SDKM" --sdk_root="$SDK" "platform-tools" "platforms;android-34" "build-tools;34.0.0"
fi
if [ ! -d "$SDK/platforms/android-34" ]; then
  echo "[ERROR] SDK platform 34 missing"
  exit 4
fi

# ---- build ----
echo "[build] gradle assembleDebug"
chmod +x "$PROJ/gradlew"
set +e
( cd "$PROJ" && ./gradlew assembleDebug --no-daemon ) > "$OUT/gradle.log" 2>&1
rc=$?
set -e
if [ $rc -ne 0 ]; then
  echo "[build] gradle exited $rc; last lines:"
  tail -n 30 "$OUT/gradle.log" || true
  exit 5
fi

APK="$PROJ/app/build/outputs/apk/debug/app-debug.apk"
if [ ! -f "$APK" ]; then
  echo "[ERROR] APK not produced"
  tail -n 20 "$OUT/gradle.log" || true
  exit 6
fi
cp "$APK" "$OUT/StarStack.apk"
echo "[build] done: $OUT/StarStack.apk"
