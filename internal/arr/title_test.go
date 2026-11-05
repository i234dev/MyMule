package arr

import "testing"

func TestReleaseTitle(t *testing.T) {
	cases := map[string]string{
		"[kad example.org] Example Movie (2009).mkv":                      "Example Movie (2009).mkv",
		"[torrent example.org] Example.Movie.2009.1080p.BluRay.x264":      "Example.Movie.2009.1080p.BluRay.x264",
		"Example.Movie.(2009).BDremux.1080p.AC3.DTS.Spa.Eng.mkv":          "Example.Movie.(2009).BDremux.1080p.AC3.DTS.Spa.Eng SPANISH ENGLISH.mkv",
		"Example Movie (2009) 1080p BluRay ESP-ING DTS 5.1 SUBS x264.mkv": "Example Movie (2009) 1080p BluRay ESP-ING DTS 5.1 SUBS x264 SPANISH ENGLISH.mkv",
		"Example Movie (2009) - 2160p [AC3, DTSHD-MA] [spa, spa].mkv":     "Example Movie (2009) - 2160p [AC3, DTSHD-MA] [spa, spa] SPANISH.mkv",
		"Example Movie (2009) (Spanish English Subs).mkv":                 "Example Movie (2009) (Spanish English Subs).mkv",
		"Example Movie (2009) (Español).avi":                              "Example Movie (2009) (Español) SPANISH.avi",
		"Example.Movie.2009.iTALiAN.AC3.DVDRip.avi":                       "Example.Movie.2009.iTALiAN.AC3.DVDRip.avi",
		"Example Movie 2009 Ita Eng x265.mkv":                             "Example Movie 2009 Ita Eng x265 ENGLISH ITALIAN.mkv",
		// Words that merely contain a tag are left alone.
		"Especial Spartans Category Engine Forage 2009.mkv": "Especial Spartans Category Engine Forage 2009.mkv",
		"Example Book.epub":                           "Example Book.epub",
		"Example Movie (1981) DVD-VHS por Fulano.avi": "Example Movie (1981) DVD-VHS por Fulano.avi",
	}
	for in, want := range cases {
		if got := releaseTitle(in); got != want {
			t.Errorf("releaseTitle(%q)\n got  %q\n want %q", in, got, want)
		}
	}
}
