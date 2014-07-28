/*
Copyright 2014 The Camlistore Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package picasa implements an importer for picasa.com accounts.
package picasa

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"

	"camlistore.org/pkg/context"
	"camlistore.org/pkg/importer"
	"camlistore.org/pkg/schema"
	"camlistore.org/pkg/schema/nodeattr"
	"camlistore.org/pkg/syncutil"

	"camlistore.org/third_party/code.google.com/p/goauth2/oauth"
	"camlistore.org/third_party/github.com/tgulacsi/picago"
)

const (
	apiURL   = "https://api.picasa.com/v2/"
	authURL  = "https://accounts.google.com/o/oauth2/auth"
	tokenURL = "https://accounts.google.com/o/oauth2/token"
	scopeURL = "https://picasaweb.google.com/data/"

	// runCompleteVersion is a cache-busting version number of the
	// importer code. It should be incremented whenever the
	// behavior of this importer is updated enough to warrant a
	// complete run.  Otherwise, if the importer runs to
	// completion, this version number is recorded on the account
	// permanode and subsequent importers can stop early.
	runCompleteVersion = "1"
)

func init() {
	importer.Register("picasa", newImporter())
}

var _ importer.ImporterSetupHTMLer = (*imp)(nil)

type imp struct {
	extendedOAuth2
}

var baseOAuthConfig = oauth.Config{
	AuthURL:  authURL,
	TokenURL: tokenURL,
	Scope:    scopeURL,

	// AccessType needs to be "offline", as the user is not here all the time;
	// ApprovalPrompt needs to be "force" to be able to get a RefreshToken
	// everytime, even for Re-logins, too.
	//
	// Source: https://developers.google.com/youtube/v3/guides/authentication#server-side-apps
	AccessType:     "offline",
	ApprovalPrompt: "force",
}

func newImporter() *imp {
	return &imp{
		newExtendedOAuth2(
			baseOAuthConfig,
			func(ctx *context.Context) (*userInfo, error) {
				u, err := picago.GetUser(ctx.HTTPClient(), "default")
				if err != nil {
					return nil, err
				}
				firstName, lastName := u.Name, ""
				i := strings.LastIndex(u.Name, " ")
				if i >= 0 {
					firstName, lastName = u.Name[:i], u.Name[i+1:]
				}
				return &userInfo{
					ID:        u.ID,
					FirstName: firstName,
					LastName:  lastName,
				}, nil
			}),
	}
}

func (*imp) AccountSetupHTML(host *importer.Host) string {
	// Picasa doesn't allow a path in the origin. Remove it.
	origin := host.ImporterBaseURL()
	if u, err := url.Parse(origin); err == nil {
		u.Path = ""
		origin = u.String()
	}

	callback := host.ImporterBaseURL() + "picasa/callback"
	return fmt.Sprintf(`
<h1>Configuring Picasa</h1>
<p>Visit <a href='https://console.developers.google.com/'>https://console.developers.google.com/</a>
and click <b>"Create Project"</b>.</p>
<p>Then under "APIs & Auth" in the left sidebar, click on "Credentials", then click the button <b>"Create new Client ID"</b>.</p>
<p>Use the following settings:</p>
<ul>
  <li>Web application</li>
  <li>Authorized JavaScript origins: <b>%s</b></li>
  <li>Authorized Redirect URI: <b>%s</b></li>
</ul>
<p>Click "Create Client ID".  Copy the "Client ID" and "Client Secret" into the boxes above.</p>
`, origin, callback)
}

// A run is our state for a given run of the importer.
type run struct {
	*importer.RunContext
	im          *imp
	incremental bool // whether we've completed a run in the past
	photoGate   *syncutil.Gate

	mu     sync.Mutex // guards anyErr
	anyErr bool
}

func (r *run) errorf(format string, args ...interface{}) {
	log.Printf(format, args...)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.anyErr = true
}

var forceFullImport, _ = strconv.ParseBool(os.Getenv("CAMLI_PICASA_FULL_IMPORT"))

func (im *imp) Run(ctx *importer.RunContext) error {
	clientId, secret, err := ctx.Credentials()
	if err != nil {
		return err
	}
	acctNode := ctx.AccountNode()
	ocfg := baseOAuthConfig
	ocfg.ClientId, ocfg.ClientSecret = clientId, secret
	token := decodeToken(acctNode.Attr(acctAttrOAuthToken))
	transport := &oauth.Transport{
		Config:    &ocfg,
		Token:     &token,
		Transport: notOAuthTransport(ctx.HTTPClient()),
	}
	ctx.Context = ctx.Context.New(context.WithHTTPClient(transport.Client()))
	r := &run{
		RunContext:  ctx,
		im:          im,
		incremental: !forceFullImport && acctNode.Attr(importer.AcctAttrCompletedVersion) == runCompleteVersion,
		photoGate:   syncutil.NewGate(3),
	}
	if err := r.importAlbums(); err != nil {
		return err
	}

	r.mu.Lock()
	anyErr := r.anyErr
	r.mu.Unlock()
	if !anyErr {
		if err := acctNode.SetAttrs(importer.AcctAttrCompletedVersion, runCompleteVersion); err != nil {
			return err
		}
	}

	return nil
}

func (r *run) importAlbums() error {
	albums, err := picago.GetAlbums(r.HTTPClient(), "default")
	if err != nil {
		return fmt.Errorf("importAlbums: error listing albums: %v", err)
	}
	albumsNode, err := r.getTopLevelNode("albums", "Albums")
	for _, album := range albums {
		if r.Context.IsCanceled() {
			return context.ErrCanceled
		}
		if err := r.importAlbum(albumsNode, album); err != nil {
			return fmt.Errorf("picasa importer: error importing album %s: %v", album, err)
		}
	}
	return nil
}

func (r *run) importAlbum(albumsNode *importer.Object, album picago.Album) (ret error) {
	if album.ID == "" {
		return errors.New("album has no ID")
	}
	albumNode, err := albumsNode.ChildPathObject(album.ID)
	if err != nil {
		return fmt.Errorf("importAlbum: error listing album: %v", err)
	}

	dateMod := schema.RFC3339FromTime(album.Updated)

	// Data reference: https://developers.google.com/picasa-web/docs/2.0/reference
	// TODO(tgulacsi): add more album info
	changes, err := albumNode.SetAttrs2(
		"picasaId", album.ID,
		nodeattr.Type, "picasaweb.google.com:album",
		nodeattr.Title, album.Title,
		nodeattr.DatePublished, schema.RFC3339FromTime(album.Published),
		nodeattr.LocationText, album.Location,
	)
	if err != nil {
		return fmt.Errorf("error setting album attributes: %v", err)
	}
	if !changes && r.incremental && albumNode.Attr(nodeattr.DateModified) == dateMod {
		return nil
	}
	defer func() {
		// Don't update DateModified on the album node until
		// we've successfully imported all the photos.
		if ret == nil {
			ret = albumNode.SetAttr(nodeattr.DateModified, dateMod)
		}
	}()

	log.Printf("Importing album %v: %v/%v (published %v, updated %v)", album.ID, album.Name, album.Title, album.Published, album.Updated)

	// TODO(bradfitz): GetPhotos does multiple HTTP requests to
	// return a slice of all photos. My "InstantUpload/Auto
	// Backup" album has 6678 photos (and growing) and this
	// currently takes like 40 seconds. Fix.
	photos, err := picago.GetPhotos(r.HTTPClient(), "default", album.ID)
	if err != nil {
		return err
	}

	log.Printf("Importing %d photos from album %q (%s)", len(photos), albumNode.Attr(nodeattr.Title),
		albumNode.PermanodeRef())

	var grp syncutil.Group
	for i := range photos {
		if r.Context.IsCanceled() {
			return context.ErrCanceled
		}
		photo := photos[i]
		r.photoGate.Start()
		grp.Go(func() error {
			defer r.photoGate.Done()
			return r.updatePhotoInAlbum(albumNode, photo)
		})
	}
	return grp.Err()
}

func (r *run) updatePhotoInAlbum(albumNode *importer.Object, photo picago.Photo) (ret error) {
	if photo.ID == "" {
		return errors.New("photo has no ID")
	}
	idFilename := photo.ID + "-" + photo.Filename()
	photoNode, err := albumNode.ChildPathObject(idFilename)
	if err != nil {
		return err
	}

	fileRefStr := photoNode.Attr(nodeattr.CamliContent)

	// Only re-download the source photo if its URL has changed.
	// Empirically this seems to work: cropping a photo in the
	// photos.google.com UI causes its URL to change. And it makes
	// sense, looking at the ugliness of the URLs with all their
	// encoded/signed state.
	const attrMediaURL = "picasaMediaURL"
	if photoNode.Attr(attrMediaURL) != photo.URL {
		log.Printf("Importing media from %v", photo.URL)
		cl := r.HTTPClient()
		resp, err := cl.Get(photo.URL)
		if err != nil {
			return fmt.Errorf("importing photo %d: %v", photo.ID, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("importing photo %d: status code = %d", photo.ID, resp.StatusCode)
		}
		fileRef, err := schema.WriteFileFromReader(r.Host.Target(), photo.Filename(), resp.Body)
		if err != nil {
			return err
		}
		fileRefStr = fileRef.String()
	}

	// TODO(tgulacsi): add more attrs (comments ?)
	// for names, see http://schema.org/ImageObject and http://schema.org/CreativeWork
	attrs := []string{
		nodeattr.CamliContent, fileRefStr,
		"picasaId", photo.ID,
		nodeattr.Title, photo.Title,
		"caption", photo.Summary,
		nodeattr.Description, photo.Description,
		nodeattr.LocationText, photo.Location,
		nodeattr.DateModified, schema.RFC3339FromTime(photo.Updated),
		nodeattr.DatePublished, schema.RFC3339FromTime(photo.Published),
	}
	if photo.Latitude != 0 || photo.Longitude != 0 {
		attrs = append(attrs,
			nodeattr.Latitude, fmt.Sprintf("%f", photo.Latitude),
			nodeattr.Longitude, fmt.Sprintf("%f", photo.Longitude),
		)
	}
	if err := photoNode.SetAttrs(attrs...); err != nil {
		return err
	}
	if err := photoNode.SetAttrValues("tag", photo.Keywords); err != nil {
		return err
	}

	// Do this last, after we're sure the "camliContent" attribute
	// has been saved successfully, because this is the one that
	// causes us to do it again in the future or not.
	if err := photoNode.SetAttrs(attrMediaURL, photo.URL); err != nil {
		return err
	}
	return nil
}

func (r *run) getTopLevelNode(path string, title string) (*importer.Object, error) {
	childObject, err := r.RootNode().ChildPathObject(path)
	if err != nil {
		return nil, err
	}

	if err := childObject.SetAttr(nodeattr.Title, title); err != nil {
		return nil, err
	}
	return childObject, nil
}
