package envdocker

// 라벨 키 — agent의 권위 소스. CreateEnv 시 컨테이너에 박아두면
// Start/Delete/RestoreFromDocker 가 docker inspect로 자급자족 가능.
const (
	LabelEnvID        = "flexctl.env_id"
	LabelRole         = "flexctl.role"         // "sidecar" | "dev"
	LabelHostname     = "flexctl.hostname"
	LabelHeadscaleURL = "flexctl.headscale_url"
	LabelTags         = "flexctl.tags"         // csv
	LabelImageRef     = "flexctl.image_ref"
	LabelGPUIndices   = "flexctl.gpu_indices"  // csv ints, "" if none
	LabelVolumeName   = "flexctl.volume_name"
)
