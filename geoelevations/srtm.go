package geoelevations

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"strings"
)

const (
	//SRTM_BASE_URL = "http://dds.cr.usgs.gov/srtm"
	SRTM_BASE_URL = "http://srtm.kurviger.de"
	SRTM1_URL     = "/SRTM1/"
	SRTM3_URL     = "/SRTM3/"
)

type Srtm struct {
	cache map[string]*SrtmFile

	srtmData SrtmData
	storage  SrtmLocalStorage
}

func NewSrtm(client *http.Client) (*Srtm, error) {
	return NewSrtmWithCustomCacheDir(client, "")
}

func NewSrtmWithCustomStorage(client *http.Client, storage SrtmLocalStorage) (*Srtm, error) {
	srtmData, err := newSrtmData(client, storage)
	if err != nil {
		return nil, err
	}

	return &Srtm{
		cache:    make(map[string]*SrtmFile),
		storage:  storage,
		srtmData: *srtmData,
	}, nil
}

func NewSrtmWithCustomCacheDir(client *http.Client, cacheDirectory string) (*Srtm, error) {
	storage, err := NewLocalFileSrtmStorage(cacheDirectory)
	if err != nil {
		return nil, err
	}
	return NewSrtmWithCustomStorage(client, storage)
}

func (self *Srtm) GetElevation(client *http.Client, latitude, longitude float64) (float64, error) {
	srtmFileName, srtmLatitude, srtmLongitude := self.getSrtmFileNameAndCoordinates(latitude, longitude)
	//log.Printf("srtmFileName for %v,%v: %s", latitude, longitude, srtmFileName)

	srtmFile, ok := self.cache[srtmFileName]
	if !ok {
		srtmFile = newSrtmFile(srtmFileName, "", srtmLatitude, srtmLongitude)
		baseUrl, srtmFileUrl := self.srtmData.GetBestSrtmUrl(srtmFileName)
		if srtmFileUrl != nil {
			srtmFile = newSrtmFile(srtmFileName, baseUrl+srtmFileUrl.Url, srtmLatitude, srtmLongitude)
		}
		self.cache[srtmFileName] = srtmFile
	}

	return srtmFile.getElevation(client, self.storage, latitude, longitude)
}

func (self *Srtm) getSrtmFileNameAndCoordinates(latitude, longitude float64) (string, float64, float64) {
	northSouth := 'S'
	if latitude >= 0 {
		northSouth = 'N'
	}

	eastWest := 'W'
	if longitude >= 0 {
		eastWest = 'E'
	}

	latPart := int(math.Abs(math.Floor(latitude)))
	lonPart := int(math.Abs(math.Floor(longitude)))

	srtmFileName := fmt.Sprintf("%s%02d%s%03d", string(northSouth), latPart, string(eastWest), lonPart)

	return srtmFileName, math.Floor(latitude), math.Floor(longitude)
}

// Struct with contents and some utility methods of a single SRTM file
type SrtmFile struct {
	latitude, longitude float64
	contents            []byte
	name                string
	fileUrl             string
	isValidSrtmFile     bool
	fileRetrieved       bool
	squareSize          int
}

func newSrtmFile(name, fileUrl string, latitude, longitude float64) *SrtmFile {
	result := SrtmFile{}
	result.name = name
	result.isValidSrtmFile = len(fileUrl) > 0
	result.latitude = latitude
	result.longitude = longitude

	result.fileUrl = fileUrl
	if !strings.HasSuffix(result.fileUrl, ".zip") {
		result.fileUrl += ".zip"
	}

	return &result
}

func (self *SrtmFile) loadContents(client *http.Client, storage SrtmLocalStorage) error {
	if !self.isValidSrtmFile || len(self.fileUrl) == 0 {
		return nil
	}

	fileName := fmt.Sprintf("%s.hgt.zip", self.name)

	bytes, err := storage.LoadFile(fileName)
	if err != nil {
		if storage.IsNotExists(err) {
			log.Printf("File %s not retrieved => retrieving: %s", fileName, self.fileUrl)
			req, err := http.NewRequest(http.MethodGet, self.fileUrl, nil)
			if err != nil {
				return err
			}
			response, err := client.Do(req)
			if err != nil {
				log.Printf("Error retrieving file: %s", err.Error())
				return err
			}

			responseBytes, err := ioutil.ReadAll(response.Body)
			if err != nil {
				return err
			}
			_ = response.Body.Close()

			if err := storage.SaveFile(fileName, responseBytes); err != nil {
				return err
			}
			log.Printf("Written %d bytes to %s", len(responseBytes), fileName)

			bytes = responseBytes
		} else {
			return err
		}
	}

	contents, err := unzipBytes(bytes)
	if err != nil {
		log.Printf("Error loading file %s: %s", fileName, err.Error())
	}
	self.contents = contents

	log.Printf("Loaded %dbytes from %s, squareSize=%d", len(self.contents), fileName, self.squareSize)

	return nil
}

func (self *SrtmFile) getElevation(client *http.Client, storage SrtmLocalStorage, latitude, longitude float64) (float64, error) {
	if !self.isValidSrtmFile || len(self.fileUrl) == 0 {
		log.Printf("Invalid file %s", self.name)
		return math.NaN(), nil
	}

	if len(self.contents) == 0 {
		log.Println("load contents")
		err := self.loadContents(client, storage)
		if err != nil {
			return math.NaN(), err
		}
	}

	if self.squareSize <= 0 {
		squareSizeFloat := math.Sqrt(float64(len(self.contents)) / 2.0)
		self.squareSize = int(squareSizeFloat)

		if squareSizeFloat != float64(self.squareSize) || self.squareSize <= 0 {
			return math.NaN(), errors.New(fmt.Sprintf("Invalid size for file %s: %d", self.name, len(self.contents)))
		}
	}

	row, column := self.getRowAndColumn(latitude, longitude)
	//log.Printf("(%f, %f) => row, column = %d, %d", latitude, longitude, row, column)
	elevation := self.getElevationFromRowAndColumn(row, column)

	return elevation, nil
}

//var total = 0
//var totals = map[string]int{
//	"valid":                0,
//	"interpolated-row-col": 0,
//	"interpolated-row":     0,
//	"interpolated-col":     0,
//	"interpolated-r1":      0,
//	"interpolated-r2":      0,
//	"interpolated-c1":      0,
//	"interpolated-c2":      0,
//}

func (self SrtmFile) getElevationFromRowAndColumn(row, column int) float64 {
	var do = func(row, column int) int {
		i := row*self.squareSize + column
		byte1 := self.contents[i*2]
		byte2 := self.contents[i*2+1]
		return int(byte1)*256 + int(byte2)
	}
	result := do(row, column)

	//total++
	//
	//if total%1000 == 0 {
	//	fmt.Printf("%#v\n", totals)
	//}

	if result < 9000 {
		// result is a valid elevation
		//totals["valid"]++
		return float64(result)
	}

	// result is a void area, we can estimate by interpolating nearby values

	/*
		Very simple interpolation algorithm:
		* We step along the row backwards and forwards until we find a valid elevation value.
		* We do the same for the column.
		* If we get to the end of the square (e.g. row >= self.squareSize, row < 0, col >= self.squareSize,
		col < 0) we give up.
		* We work out a value for the estimated elevation by simple geometry.
		* If we have success from both row and column, we return the average of the elevation value obtained from
		both methods.
	*/

	// r => row
	// c => col
	// 1 => reduced index
	// 2 => increased index
	// i => the index of the row / column
	// v => the elevation
	// b => was the lookup successful?
	var ri1, ri2, ci1, ci2 int
	var rv1, rv2, cv1, cv2 int
	var rb1, rb2, cb1, cb2 bool

	for ri1 = row - 1; ri1 >= 0; ri1-- {
		rv1 = do(ri1, column)
		if rv1 < 9000 {
			rb1 = true
			break
		}
	}

	for ri2 = row + 1; ri2 < self.squareSize; ri2++ {
		rv2 = do(ri2, column)
		if rv2 < 9000 {
			rb2 = true
			break
		}
	}

	for ci1 = column - 1; ci1 >= 0; ci1-- {
		cv1 = do(row, ci1)
		if cv1 < 9000 {
			cb1 = true
			break
		}
	}

	for ci2 = column + 1; ci2 < self.squareSize; ci2++ {
		cv2 = do(row, ci2)
		if cv2 < 9000 {
			cb2 = true
			break
		}
	}

	var rb, cb bool
	var rv, cv float64
	if rb1 && rb2 {
		rb = true
		rowCount := ri2 - ri1
		eleDelta := rv2 - rv1
		elePerRow := float64(eleDelta) / float64(rowCount)
		rv = float64(rv1) + (float64(row-ri1) * elePerRow)
	}
	if cb1 && cb2 {
		cb = true
		colCount := ci2 - ci1
		eleDelta := cv2 - cv1
		elePerCol := float64(eleDelta) / float64(colCount)
		cv = float64(cv1) + (float64(column-ci1) * elePerCol)
	}
	if rb && cb {
		//fmt.Printf(
		//	"row %d: %d (%dm) -> %d (%dm) => (%dm), col %d: %d (%dm) -> %d (%dm) => (%dm)... RESULT: %d\n",
		//	row,
		//	ri1, rv1,
		//	ri2, rv2,
		//	int(rv),
		//	column,
		//	ci1, cv1,
		//	ci2, cv2,
		//	int(cv),
		//	int((rv+cv)/2.0),
		//)
		//totals["interpolated-row-col"]++
		return (rv + cv) / 2.0
	}
	if rb {
		//fmt.Printf(
		//	"row %d: %d (%dm) -> %d (%dm) - RESULT = %dm [col %d failed: %d (%dm) -> %d (%dm)]\n",
		//	row,
		//	ri1, rv1,
		//	ri2, rv2,
		//	int(rv),
		//	column,
		//	ci1, cv1,
		//	ci2, cv2,
		//)
		//totals["interpolated-row"]++
		return rv
	}
	if cb {
		//fmt.Printf(
		//	"col %d: %d (%dm) -> %d (%dm) - RESULT = %dm [row %d failed: %d (%dm) -> %d (%dm)]\n",
		//	column,
		//	ci1, cv1,
		//	ci2, cv2,
		//	int(cv),
		//	row,
		//	ri1, rv1,
		//	ri2, rv2,
		//)
		//totals["interpolated-col"]++
		return cv
	}
	if rb1 {
		//totals["interpolated-r1"]++
		return float64(rv1)
	}
	if rb2 {
		//totals["interpolated-r2"]++
		return float64(rv2)
	}
	if cb1 {
		//totals["interpolated-c1"]++
		return float64(cv1)
	}
	if cb2 {
		//totals["interpolated-c2"]++
		return float64(cv2)
	}
	return math.NaN()

}

func (self SrtmFile) getRowAndColumn(latitude, longitude float64) (int, int) {
	row := int((self.latitude + 1.0 - latitude) * (float64(self.squareSize - 1.0)))
	column := int((longitude - self.longitude) * (float64(self.squareSize - 1.0)))
	//log.Printf("squareSize=%v", self.squareSize)
	//log.Printf("row, column = %v, %v", row, column)
	return row, column
}

// ----------------------------------------------------------------------------------------------------
// Misc util functions
// ----------------------------------------------------------------------------------------------------

func LoadSrtmData(client *http.Client) (*SrtmData, error) {
	result := new(SrtmData)

	var err error
	result.Srtm1BaseUrl = SRTM_BASE_URL + SRTM1_URL
	result.Srtm1, err = getLinksFromUrl(client, result.Srtm1BaseUrl, result.Srtm1BaseUrl, 0)
	if err != nil {
		return nil, err
	}

	result.Srtm3BaseUrl = SRTM_BASE_URL + SRTM3_URL
	result.Srtm3, err = getLinksFromUrl(client, result.Srtm3BaseUrl, result.Srtm3BaseUrl, 0)
	if err != nil {
		return nil, err
	}

	return result, nil
}

func getLinksFromUrl(client *http.Client, baseUrl, url string, depth int) ([]SrtmUrl, error) {
	if depth >= 2 {
		return []SrtmUrl{}, nil
	}

	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}

	result := make([]SrtmUrl, 0)

	urls := getLinksFromHtmlDocument(resp.Body)
	for _, tmpURL := range urls {
		if strings.HasSuffix(tmpURL, "/index.html") {
			tmpURL = strings.Replace(tmpURL, "/index.html", "", 1)
		}
		urlLowercase := strings.ToLower(tmpURL)
		if strings.HasSuffix(urlLowercase, ".hgt.zip") {
			parts := strings.Split(tmpURL, "/")
			name := parts[len(parts)-1]
			name = strings.Replace(name, ".hgt.zip", "", -1)
			u := strings.Replace(fmt.Sprintf("%s/%s", url, tmpURL), baseUrl, "", 1)
			srtmUrl := SrtmUrl{Name: name, Url: u}
			result = append(result, srtmUrl)
			log.Printf("> %s/%s -> %s\n", url, tmpURL, tmpURL)
		} else if len(urlLowercase) > 0 && urlLowercase[0] != '/' && !strings.HasPrefix(urlLowercase, "http") && !strings.HasSuffix(urlLowercase, ".jpg") {
			newLinks, err := getLinksFromUrl(client, baseUrl, fmt.Sprintf("%s/%s", url, tmpURL), depth+1)
			if err != nil {
				return nil, err
			}
			result = append(result, newLinks...)
			log.Printf("> %s\n", tmpURL)
		}
	}

	return result, nil
}

func getLinksFromHtmlDocument(html io.ReadCloser) []string {
	result := make([]string, 10)

	decoder := xml.NewDecoder(html)
	for token, _ := decoder.Token(); token != nil; token, _ = decoder.Token() {
		switch typedToken := token.(type) {
		case xml.StartElement:
			for _, attr := range typedToken.Attr {
				if strings.ToLower(attr.Name.Local) == "href" {
					result = append(result, strings.Trim(attr.Value, " \r\t\n"))
				}
			}
		}
	}

	return result
}
