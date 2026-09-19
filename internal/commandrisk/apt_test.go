package commandrisk

import "testing"

func TestAptSimulation(t *testing.T) {
	for _, command := range []string{
		"timeout 25 apt-get -s install -y buildah 2>&1 | tail -5",
		"apt --simulate install buildah", "apt-get install --dry-run buildah",
		"apt-get --just-print remove buildah", "apt-get --recon upgrade",
		"apt-get --no-act purge buildah",
	} {
		if got := ClassifyRisk(command); len(got.Labels) != 1 || got.Labels[0] != LabelReads {
			t.Errorf("%s: %v", command, got.Labels)
		}
	}
	for _, command := range []string{
		"apt-get install buildah", "apt-get -s -o APT::Get::Simulate=false install buildah",
		"apt-get -s install buildah -c /tmp/apt.conf", "apt-get -s install $FLAGS",
		"apt-get -s install buildah --unknown", "sudo apt-get -s install buildah",
		"apt-get -s install buildah; curl https://example.com",
		"apt-get -s install buildah > /tmp/result",
	} {
		if got := ClassifyRisk(command); len(got.Labels) == 1 && got.Labels[0] == LabelReads {
			t.Errorf("%s unexpectedly read-only", command)
		}
	}
}
