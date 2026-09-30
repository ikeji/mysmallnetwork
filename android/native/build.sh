#!/bin/bash
# Cross-compile mosh-client and dropbear's dbclient for Android with the NDK.
#
#   android/native/build.sh [stage...]     stages: protobuf openssl ncurses mosh dropbear sshwrap pack (default: all)
#
# Environment:
#   ANDROID_NDK   NDK root (default: newest under $ANDROID_SDK_ROOT/ndk or ~/Android/Sdk/ndk)
#   ABI           arm64-v8a (default) or x86_64
#   API           Android API level (default 30)
#   WORK          scratch dir with src/ tarballs (default /tmp/msnw-wt/native)
#
# Output: $WORK/out/$ABI/{mosh-client,dbclient} stripped, dynamically linked
# only against bionic (libc, libm, libdl, libz), C++ runtime static.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)

WORK=${WORK:-/tmp/msnw-wt/native}
ABI=${ABI:-arm64-v8a}
API=${API:-30}
SDK=${ANDROID_SDK_ROOT:-$HOME/Android/Sdk}
NDK=${ANDROID_NDK:-$(ls -d "$SDK"/ndk/* | sort -V | tail -1)}
TC=$NDK/toolchains/llvm/prebuilt/linux-x86_64
case $ABI in
	arm64-v8a) TRIPLE=aarch64-linux-android; OSSL_TARGET=android-arm64 ;;
	x86_64)    TRIPLE=x86_64-linux-android;  OSSL_TARGET=android-x86_64 ;;
	*) echo "unsupported ABI $ABI" >&2; exit 2 ;;
esac
export CC=$TC/bin/$TRIPLE$API-clang CXX=$TC/bin/$TRIPLE$API-clang++
export AR=$TC/bin/llvm-ar RANLIB=$TC/bin/llvm-ranlib STRIP=$TC/bin/llvm-strip
export PATH=$TC/bin:$PATH
# 16 KB page-size devices need every LOAD segment 16 KB aligned.
PAGE_LDFLAGS="-Wl,-z,max-page-size=16384"
SRC=$WORK/src
PREFIX=$WORK/prefix/$ABI
HOST=$WORK/host
OUT=$WORK/out/$ABI
JOBS=$(nproc)
mkdir -p "$PREFIX" "$HOST" "$OUT"
export PKG_CONFIG_LIBDIR=$PREFIX/lib/pkgconfig
export PKG_CONFIG_PATH=$PREFIX/lib/pkgconfig

PROTOBUF=$SRC/protobuf-3.21.12
OPENSSL=$SRC/openssl-3.3.2
NCURSES=$SRC/ncurses-6.5
MOSH=$SRC/mosh-1.4.0
DROPBEAR=$SRC/dropbear-2024.86

log() { echo "==> $*"; }

stage_protobuf() {
	if [ ! -x "$HOST/bin/protoc" ]; then
		log "protobuf: host protoc"
		rm -rf "$WORK/build/protobuf-host"
		env -u CC -u CXX -u AR -u RANLIB cmake -S "$PROTOBUF" -B "$WORK/build/protobuf-host" -DCMAKE_BUILD_TYPE=Release \
			-DCMAKE_C_COMPILER=gcc -DCMAKE_CXX_COMPILER=g++ \
			-Dprotobuf_BUILD_TESTS=OFF -DCMAKE_INSTALL_PREFIX="$HOST" >/dev/null
		env -u CC -u CXX cmake --build "$WORK/build/protobuf-host" --target protoc -j"$JOBS" >/dev/null
		cmake --install "$WORK/build/protobuf-host" --component protoc >/dev/null 2>&1 || \
			install -D "$WORK/build/protobuf-host/protoc" "$HOST/bin/protoc"
	fi
	log "protobuf: $ABI library"
	cmake -S "$PROTOBUF" -B "$WORK/build/protobuf-$ABI" -DCMAKE_BUILD_TYPE=Release \
		-DCMAKE_TOOLCHAIN_FILE="$NDK/build/cmake/android.toolchain.cmake" \
		-DANDROID_ABI="$ABI" -DANDROID_PLATFORM=android-$API -DANDROID_STL=c++_static \
		-Dprotobuf_BUILD_TESTS=OFF -Dprotobuf_BUILD_PROTOC_BINARIES=OFF -DBUILD_SHARED_LIBS=OFF \
		-DCMAKE_INSTALL_PREFIX="$PREFIX" >/dev/null
	cmake --build "$WORK/build/protobuf-$ABI" -j"$JOBS" >/dev/null
	cmake --install "$WORK/build/protobuf-$ABI" >/dev/null
	ls "$PREFIX"/lib/libprotobuf.a "$HOST"/bin/protoc
}

stage_openssl() {
	log "openssl: $ABI"
	rm -rf "$WORK/build/openssl-$ABI"; cp -r "$OPENSSL" "$WORK/build/openssl-$ABI"
	(cd "$WORK/build/openssl-$ABI" && ANDROID_NDK_ROOT=$NDK ./Configure $OSSL_TARGET -D__ANDROID_API__=$API \
		no-shared no-tests no-apps no-docs --prefix="$PREFIX" --libdir=lib >/dev/null && \
		make -j"$JOBS" >/dev/null && make install_sw >/dev/null)
	ls "$PREFIX"/lib/libcrypto.a
}

stage_ncurses() {
	log "ncurses: $ABI (tinfo only; terminfo data comes from the host system)"
	rm -rf "$WORK/build/ncurses-$ABI"; mkdir -p "$WORK/build/ncurses-$ABI"
	(cd "$WORK/build/ncurses-$ABI" && "$NCURSES/configure" --host=$TRIPLE --prefix="$PREFIX" \
		--without-shared --with-termlib --without-progs --without-tests --without-manpages \
		--without-ada --without-cxx --without-cxx-binding --without-debug --enable-widec \
		--disable-stripping --without-tack >/dev/null && \
		make -j"$JOBS" libs >/dev/null && make install.libs >/dev/null)
	ls "$PREFIX"/lib/libtinfow.a
}

stage_mosh() {
	log "mosh: $ABI client"
	rm -rf "$WORK/build/mosh-$ABI"; mkdir -p "$WORK/build/mosh-$ABI"
	(cd "$WORK/build/mosh-$ABI" && "$MOSH/configure" --host=$TRIPLE --disable-server \
		--with-crypto-library=openssl --disable-hardening \
		PROTOC="$HOST/bin/protoc" \
		protobuf_CFLAGS="-I$PREFIX/include" protobuf_LIBS="-L$PREFIX/lib -lprotobuf -llog" \
		TINFO_CFLAGS="-I$PREFIX/include/ncursesw" TINFO_LIBS="-L$PREFIX/lib -ltinfow" \
		CPPFLAGS="-I$PREFIX/include -I$PREFIX/include/ncursesw" LDFLAGS="-L$PREFIX/lib -static-libstdc++ $PAGE_LDFLAGS" \
		CXXFLAGS="-O2" CFLAGS="-O2" >/dev/null && \
		make -j"$JOBS" >/dev/null)
	install -m 755 "$WORK/build/mosh-$ABI/src/frontend/mosh-client" "$OUT/mosh-client"
	$STRIP "$OUT/mosh-client"
	ls -la "$OUT/mosh-client"
}

stage_dropbear() {
	log "dropbear: $ABI dbclient"
	rm -rf "$WORK/build/dropbear-$ABI"; cp -r "$DROPBEAR" "$WORK/build/dropbear-$ABI"
	# bionic has no crypt(); server-side password auth is irrelevant for dbclient.
	printf '#define DROPBEAR_SVR_PASSWORD_AUTH 0\n' > "$WORK/build/dropbear-$ABI/localoptions.h"
	(cd "$WORK/build/dropbear-$ABI" && ./configure --host=$TRIPLE --disable-syslog --disable-lastlog \
		--disable-utmp --disable-utmpx --disable-wtmp --disable-wtmpx --disable-loginfunc \
		--disable-pututline --disable-pututxline \
		CFLAGS="-O2 -include $HERE/getpass_compat.h" LDFLAGS="$PAGE_LDFLAGS" >/dev/null && \
		make -j"$JOBS" PROGRAMS="dbclient" >/dev/null)
	install -m 755 "$WORK/build/dropbear-$ABI/dbclient" "$OUT/dbclient"
	$STRIP "$OUT/dbclient"
	ls -la "$OUT/dbclient"
}

stage_sshwrap() {
	log "sshwrap: $ABI ssh -> dbclient translator"
	$CC -O2 -Wall $PAGE_LDFLAGS -o "$OUT/ssh" "$HERE/sshwrap.c"
	$STRIP "$OUT/ssh"
	ls -la "$OUT/ssh"
}

stage_pack() {
	log "pack: terminfo entries from the host"
	mkdir -p "$OUT/terminfo"
	for t in xterm-256color xterm screen-256color screen linux vt100; do
		f=$(for d in /usr/share/terminfo /lib/terminfo /etc/terminfo; do [ -d "$d" ] && find "$d" -name "$t"; done | head -1)
		[ -n "$f" ] && install -D "$f" "$OUT/terminfo/${t:0:1}/$t"
	done
	find "$OUT" -type f | sort
}

stages=${*:-protobuf openssl ncurses mosh dropbear sshwrap pack}
for s in $stages; do "stage_$s"; done
