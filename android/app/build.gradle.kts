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

android {
    namespace = "ma.ikeji.msnw"
    compileSdk = 35

    defaultConfig {
        applicationId = "ma.ikeji.msnw"
        minSdk = 26
        targetSdk = 35
        versionCode = 1
        versionName = "0.1"
        ndk { abiFilters += listOf("arm64-v8a") }
    }
    buildTypes {
        release {
            isMinifyEnabled = false
            signingConfig = signingConfigs.getByName("debug")
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
    from(file("$nativeOut/arm64-v8a/ssh")) { rename { "libssh.so" } }
    into(layout.projectDirectory.dir("src/main/jniLibs/arm64-v8a"))
    doFirst {
        listOf(goBin.resolve("linux-arm64/msnw"), file("$nativeOut/arm64-v8a/mosh-client")).forEach {
            if (!it.exists()) throw GradleException("missing $it: run 'make cross' and android/native/build.sh first")
        }
    }
}
val copyTerminfo by tasks.registering(Copy::class) {
    from(file("$nativeOut/arm64-v8a/terminfo"))
    into(layout.projectDirectory.dir("src/main/assets/terminfo"))
}
tasks.named("preBuild") { dependsOn(copyNative, copyTerminfo) }

dependencies {
    implementation(project(":terminal-view"))
    implementation("androidx.core:core-ktx:1.15.0")
    implementation("androidx.appcompat:appcompat:1.7.0")
    implementation("androidx.webkit:webkit:1.12.1")
}
