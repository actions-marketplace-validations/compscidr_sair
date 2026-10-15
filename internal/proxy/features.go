package proxy

import "strings"

// modernSdk is assumed when no device is visible to describe.
const modernSdk = 36

// FeaturesForSdk builds the host:features reply for a device at the given API
// level. Real adb intersects the host's features with the device's; the proxy
// never sees adbd's list, so it derives it from the API level at which adbd
// gained each feature. Advertising a feature the device lacks makes adb use a
// protocol the device closes on: shell_v2 on an API 17 phone loops forever in
// "- waiting for device -".
//
// An unknown level (0: the device-source has not read the phone's build props
// yet) gets the legacy set, which every device speaks. Assuming modern there
// hung CI on an API 17 phone: adb fetches features once per command, so one
// answer given before the props read landed looped that command for good.
//
// abb_exec matters for installs: without it adb streams an APK into
// `exec:cmd package install`, a cmd process on the phone, which on some
// devices (a Galaxy A06, sair#79) occasionally ends without its result
// reaching the host, so the install hangs. With it adb uses the same binder
// path as direct adb. The tunnel carries it like any other service. It is
// advertised from API 30 only: the release that added it is not pinned down,
// and a device without it would fail installs, while a device that has it
// but is not offered it just keeps the cmd path. abb (interactive) is still
// left out: nothing needs it.
func FeaturesForSdk(sdk int32) string {
	features := []string{"openscreen_mdns"} // host-side, device-independent
	if sdk >= 24 {
		features = append(features, "shell_v2", "cmd")
	}
	if sdk >= 26 {
		features = append(features, "stat_v2", "ls_v2")
	}
	if sdk >= 28 {
		features = append(features, "fixed_push_mkdir")
	}
	if sdk >= 29 {
		features = append(features, "apex", "fixed_push_symlink_timestamp")
	}
	if sdk >= 30 {
		features = append(features, "abb_exec", "remount_shell", "track_app",
			"sendrecv_v2", "sendrecv_v2_brotli", "sendrecv_v2_lz4", "sendrecv_v2_zstd", "sendrecv_v2_dry_run_send")
	}
	return strings.Join(features, ",")
}
