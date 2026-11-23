plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

// Native programs are built outside Gradle: the Go binary by "make cross" and
// mosh-client / dbclient / ssh by android/native/build.sh. They are packaged
// as "shared libraries" so Android extracts them to a directory the app may
// execute from.
val nativeOut = System.getenv("MSNW_NATIVE_OUT") ?: "/tmp/msnw-wt/native/out"
val goBin = rootProject.file("../bin")

fun gitVersion(): String = try {
    val p = ProcessBuilder("git", "describe", "--tags", "--always", "--dirty")
        .directory(rootProject.file(".."))
        .redirectErrorStream(true)
        .start()
    val out = p.inputStream.bufferedReader().readText().trim()
    if (p.waitFor() == 0 && out.isNotEmpty()) out else "dev"
} catch (_: Exception) {
    "dev"
}

val appVersion = System.getenv("MSNW_VERSION")?.takeIf { it.isNotBlank() } ?: gitVersion()
val appVersionCode = Regex("""^v?(\d+)\.(\d+)\.(\d+)""").find(appVersion)?.let { m ->
    val (maj, min, pat) = m.destructured
    maj.toInt() * 1_000_000 + min.toInt() * 1_000 + pat.toInt()
} ?: 1

val debugJniLibs = layout.buildDirectory.dir("debugJniLibs")

android {
    namespace = "ma.ikeji.msnw"
    compileSdk = 35

    defaultConfig {
        applicationId = "ma.ikeji.msnw"
        minSdk = 26
        targetSdk = 35
        versionCode = appVersionCode
        versionName = appVersion
        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"
    }
    // Release signing from the environment (MSNW_KEYSTORE, MSNW_KEYSTORE_PASSWORD,
    // MSNW_KEY_ALIAS, MSNW_KEY_PASSWORD); without it, releases use the debug key.
    val ks = System.getenv("MSNW_KEYSTORE")
    if (ks != null) {
        signingConfigs.create("release") {
            storeFile = file(ks)
            storePassword = System.getenv("MSNW_KEYSTORE_PASSWORD")
            keyAlias = System.getenv("MSNW_KEY_ALIAS") ?: "msnw"
            keyPassword = System.getenv("MSNW_KEY_PASSWORD") ?: System.getenv("MSNW_KEYSTORE_PASSWORD")
        }
    }
    buildTypes {
        debug {
            ndk { abiFilters += listOf("arm64-v8a", "x86_64") }
        }
        release {
            isMinifyEnabled = false
            ndk { abiFilters += listOf("arm64-v8a") }
            signingConfig = if (ks != null) signingConfigs.getByName("release") else signingConfigs.getByName("debug")
        }
    }
    sourceSets {
        getByName("debug") {
            jniLibs.srcDir(debugJniLibs)
        }
    }
    packaging {
        jniLibs { useLegacyPackaging = true } // extract to nativeLibraryDir so they can be executed
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions { jvmTarget = "17" }
}

val copyNative by tasks.registering(Copy::class) {
    description = "Copies the Go and native binaries into jniLibs and terminfo into assets"
    from(goBin.resolve("linux-arm64/msnw")) { rename { "libmsnw.so" } }
    from(file("$nativeOut/arm64-v8a/mosh-client")) { rename { "libmosh-client.so" } }
    from(file("$nativeOut/arm64-v8a/dbclient")) { rename { "libdbclient.so" } }
    from(file("$nativeOut/arm64-v8a/dropbearkey")) { rename { "libdropbearkey.so" } }
    from(file("$nativeOut/arm64-v8a/ssh")) { rename { "libssh.so" } }
    into(layout.projectDirectory.dir("src/main/jniLibs/arm64-v8a"))
    doFirst {
        listOf(goBin.resolve("linux-arm64/msnw"), file("$nativeOut/arm64-v8a/mosh-client")).forEach {
            if (!it.exists()) throw GradleException("missing $it: run 'make cross' and android/native/build.sh first")
        }
    }
}
val copyDebugX86Native by tasks.registering(Copy::class) {
    description = "Copies the x86_64 Go binary into debug jniLibs for emulator testing"
    from(goBin.resolve("linux-amd64/msnw")) { rename { "libmsnw.so" } }
    into(debugJniLibs.map { it.dir("x86_64") })
}
val copyTerminfo by tasks.registering(Copy::class) {
    from(file("$nativeOut/arm64-v8a/terminfo"))
    into(layout.projectDirectory.dir("src/main/assets/terminfo"))
}
tasks.named("preBuild") { dependsOn(copyNative, copyDebugX86Native, copyTerminfo) }

dependencies {
    implementation(project(":terminal-view"))
    implementation("androidx.core:core-ktx:1.15.0")
    implementation("androidx.appcompat:appcompat:1.7.0")
    implementation("androidx.webkit:webkit:1.12.1")
    androidTestImplementation("junit:junit:4.13.2")
    androidTestImplementation("androidx.test:runner:1.6.2")
}
