// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	pkgdomain "github.com/tenseleyFlow/shithub/internal/packages"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

const packageUploadBodySlack = 10 << 20

type repoPackageUploadForm struct {
	Name        string
	Version     string
	Description string
}

type repoPackageFileView struct {
	ID          int64
	Version     string
	Filename    string
	ContentType string
	SizeLabel   string
	DownloadURL string
	CreatedAt   time.Time
}

type repoPackageView struct {
	ID            int64
	Name          string
	PackageType   string
	Description   string
	LatestVersion string
	PackageBytes  string
	VersionCount  int
	FileCount     int
	Files         []repoPackageFileView
	DeleteURL     string
	UpdatedAt     time.Time
	CreatedAt     time.Time
}

func (h *Handlers) repoTabPackages(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	h.renderRepoPackagesPage(w, r, row, owner.Username, "", packageNotice(r.URL.Query().Get("notice")), repoPackageUploadForm{})
}

func (h *Handlers) repoPackageUpload(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	if !row.OwnerOrgID.Valid {
		h.renderRepoPackagesPage(w, r, row, owner.Username, "Packages are currently available for organization repositories.", "", repoPackageUploadForm{})
		return
	}
	if h.d.ObjectStore == nil {
		h.renderRepoPackagesPage(w, r, row, owner.Username, "Package uploads are unavailable until object storage is configured.", "", repoPackageUploadForm{})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, pkgdomain.MaxPackageFileBytes+packageUploadBodySlack)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		h.renderRepoPackagesPage(w, r, row, owner.Username, "Package upload is too large or malformed.", "", repoPackageUploadForm{})
		return
	}
	form := repoPackageUploadForm{
		Name:        strings.TrimSpace(r.PostFormValue("name")),
		Version:     strings.TrimSpace(r.PostFormValue("version")),
		Description: strings.TrimSpace(r.PostFormValue("description")),
	}
	file, header, err := r.FormFile("package_file")
	if err != nil {
		h.renderRepoPackagesPage(w, r, row, owner.Username, "Choose a package file to upload.", "", form)
		return
	}
	defer file.Close()
	filename := cleanPackageFilename(header.Filename)
	if filename == "" {
		h.renderRepoPackagesPage(w, r, row, owner.Username, "Package file name is invalid.", "", form)
		return
	}
	sizeBytes := header.Size
	if sizeBytes < 0 || sizeBytes > pkgdomain.MaxPackageFileBytes {
		h.renderRepoPackagesPage(w, r, row, owner.Username, "Package file exceeds the upload limit.", "", form)
		return
	}
	if msg, status, ok := h.packageQuotaBlocker(r, row, sizeBytes); ok {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		h.renderRepoPackagesPage(w, r, row, owner.Username, msg, "", form)
		return
	}
	objectKey, err := pkgdomain.NewObjectKey(row.ID, form.Name, form.Version, filename)
	if err != nil {
		h.renderRepoPackagesPage(w, r, row, owner.Username, packageInputMessage(err), "", form)
		return
	}
	contentType := packageContentType(header.Header.Get("Content-Type"), filename)
	put, err := h.d.ObjectStore.Put(r.Context(), objectKey, file, storage.PutOpts{
		ContentType:   contentType,
		IfNoneMatch:   "*",
		ContentLength: sizeBytes,
	})
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo packages: object put failed", "repo_id", row.ID, "object_key", objectKey, "error", err)
		h.renderRepoPackagesPage(w, r, row, owner.Username, "Package upload failed.", "", form)
		return
	}
	if put.Size >= 0 {
		sizeBytes = put.Size
	}
	if _, err := pkgdomain.PublishFile(r.Context(), pkgdomain.Deps{Pool: h.d.Pool}, pkgdomain.PublishInput{
		RepoID:      row.ID,
		Name:        form.Name,
		Version:     form.Version,
		Description: form.Description,
		Filename:    filename,
		ObjectKey:   objectKey,
		ContentType: contentType,
		SizeBytes:   sizeBytes,
		ETag:        put.ETag,
		ActorUserID: middleware.CurrentUserFromContext(r.Context()).ID,
	}); err != nil {
		_ = h.d.ObjectStore.Delete(r.Context(), objectKey)
		h.renderRepoPackagesPage(w, r, row, owner.Username, packageInputMessage(err), "", form)
		return
	}
	h.recalculatePackageUsage(r, row)
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/packages?notice=uploaded", http.StatusSeeOther)
}

func (h *Handlers) repoPackageDownload(w http.ResponseWriter, r *http.Request) {
	row, _, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	if h.d.ObjectStore == nil {
		h.d.Render.HTTPError(w, r, http.StatusServiceUnavailable, "package storage unavailable")
		return
	}
	fileID, err := strconv.ParseInt(chi.URLParam(r, "fileID"), 10, 64)
	if err != nil || fileID <= 0 {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	file, err := pkgdomain.GetRepoPackageFile(r.Context(), pkgdomain.Deps{Pool: h.d.Pool}, row.ID, fileID)
	if err != nil {
		if errors.Is(err, pkgdomain.ErrFileNotFound) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
			return
		}
		h.d.Logger.WarnContext(r.Context(), "repo packages: file lookup failed", "repo_id", row.ID, "file_id", fileID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	body, meta, err := h.d.ObjectStore.Get(r.Context(), file.ObjectKey)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
			return
		}
		h.d.Logger.WarnContext(r.Context(), "repo packages: object get failed", "repo_id", row.ID, "file_id", fileID, "object_key", file.ObjectKey, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	defer body.Close()
	contentType := file.ContentType
	if contentType == "" {
		contentType = meta.ContentType
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": file.Filename}))
	if meta.ETag != "" {
		w.Header().Set("ETag", meta.ETag)
	}
	if file.SizeBytes > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(file.SizeBytes, 10))
	}
	if _, err := io.Copy(w, body); err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo packages: object stream failed", "repo_id", row.ID, "file_id", fileID, "error", err)
	}
}

func (h *Handlers) repoPackageDelete(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	packageID, err := strconv.ParseInt(chi.URLParam(r, "packageID"), 10, 64)
	if err != nil || packageID <= 0 {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	keys, err := pkgdomain.DeleteRepoPackage(r.Context(), pkgdomain.Deps{Pool: h.d.Pool}, row.ID, packageID)
	if err != nil {
		if errors.Is(err, pkgdomain.ErrPackageNotFound) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
			return
		}
		h.d.Logger.WarnContext(r.Context(), "repo packages: delete failed", "repo_id", row.ID, "package_id", packageID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if h.d.ObjectStore != nil {
		for _, key := range keys {
			if err := h.d.ObjectStore.Delete(r.Context(), key); err != nil {
				h.d.Logger.WarnContext(r.Context(), "repo packages: object delete failed", "repo_id", row.ID, "package_id", packageID, "object_key", key, "error", err)
			}
		}
	}
	h.recalculatePackageUsage(r, row)
	http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/packages?notice=deleted", http.StatusSeeOther)
}

func (h *Handlers) renderRepoPackagesPage(w http.ResponseWriter, r *http.Request, row reposdb.Repo, ownerSlug, errorMessage, notice string, form repoPackageUploadForm) {
	pkgRows, err := pkgdomain.ListRepoPackages(r.Context(), pkgdomain.Deps{Pool: h.d.Pool}, row.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo packages: list failed", "repo_id", row.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	views := make([]repoPackageView, 0, len(pkgRows))
	for _, pkg := range pkgRows {
		files, err := pkgdomain.ListRepoPackageFiles(r.Context(), pkgdomain.Deps{Pool: h.d.Pool}, row.ID, pkg.ID)
		if err != nil {
			h.d.Logger.WarnContext(r.Context(), "repo packages: list files failed", "repo_id", row.ID, "package_id", pkg.ID, "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
		fileViews := make([]repoPackageFileView, 0, len(files))
		for _, file := range files {
			fileViews = append(fileViews, repoPackageFileView{
				ID:          file.ID,
				Version:     file.Version,
				Filename:    file.Filename,
				ContentType: file.ContentType,
				SizeLabel:   formatPackageBytes(file.SizeBytes),
				DownloadURL: fmt.Sprintf("/%s/%s/packages/files/%d/download", ownerSlug, row.Name, file.ID),
				CreatedAt:   file.CreatedAt.Time,
			})
		}
		views = append(views, repoPackageView{
			ID:            pkg.ID,
			Name:          pkg.Name,
			PackageType:   pkg.PackageType,
			Description:   pkg.Description,
			LatestVersion: pkg.LatestVersion,
			PackageBytes:  formatPackageBytes(pkg.PackageBytes),
			VersionCount:  int(pkg.VersionCount),
			FileCount:     int(pkg.FileCount),
			Files:         fileViews,
			DeleteURL:     fmt.Sprintf("/%s/%s/packages/%d/delete", ownerSlug, row.Name, pkg.ID),
			UpdatedAt:     pkg.UpdatedAt.Time,
			CreatedAt:     pkg.CreatedAt.Time,
		})
	}
	canPublish, disabledReason := h.packagePublishState(r, row)
	packageStorage := ""
	if row.OwnerOrgID.Valid {
		packageStorage = h.packageStorageLabel(r, row.OwnerOrgID.Int64)
	}
	data := h.repoHeaderData(r, row, ownerSlug, "packages")
	data["Title"] = "Packages · " + row.Name
	data["Packages"] = views
	data["CanPublishPackages"] = canPublish
	data["PublishDisabledReason"] = disabledReason
	data["PackageStorageLabel"] = packageStorage
	data["Error"] = errorMessage
	data["Notice"] = notice
	data["UploadForm"] = form
	data["MaxPackageFileSize"] = formatPackageBytes(pkgdomain.MaxPackageFileBytes)
	if err := h.d.Render.RenderPage(w, r, "repo/packages", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo packages render", "error", err)
	}
}

func (h *Handlers) packagePublishState(r *http.Request, row reposdb.Repo) (bool, string) {
	if h.d.ObjectStore == nil {
		return false, "Package uploads are disabled until object storage is configured."
	}
	if !row.OwnerOrgID.Valid {
		return false, "Packages are currently available for organization repositories."
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.IsAnonymous() {
		return false, "Sign in to publish packages."
	}
	if !policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, viewer.PolicyActor(), policy.ActionRepoWrite, policy.NewRepoRefFromRepo(row)).Allow {
		return false, "You need write access to publish packages."
	}
	return true, ""
}

func (h *Handlers) packageQuotaBlocker(r *http.Request, row reposdb.Repo, sizeBytes int64) (string, int, bool) {
	if sizeBytes <= 0 || !row.OwnerOrgID.Valid {
		return "", 0, false
	}
	now := time.Now().UTC()
	periodStart, periodEnd := orgbilling.MonthlyUsagePeriod(now)
	counters, err := orgbilling.RecalculateOrgUsageCounters(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, row.OwnerOrgID.Int64, periodStart, periodEnd)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo packages: quota recalc failed", "repo_id", row.ID, "org_id", row.OwnerOrgID.Int64, "error", err)
		return "Package storage quota check failed.", http.StatusInternalServerError, true
	}
	check, err := entitlements.CheckOrgStorageQuota(r.Context(), entitlements.Deps{Pool: h.d.Pool}, row.OwnerOrgID.Int64, counters.RepoStorageBytes+counters.ObjectStorageBytes, sizeBytes)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo packages: quota check failed", "repo_id", row.ID, "org_id", row.OwnerOrgID.Int64, "error", err)
		return "Package storage quota check failed.", http.StatusInternalServerError, true
	}
	if check.Allowed {
		return "", 0, false
	}
	return check.Message(), check.HTTPStatus(), true
}

func (h *Handlers) packageStorageLabel(r *http.Request, orgID int64) string {
	now := time.Now().UTC()
	periodStart, periodEnd := orgbilling.MonthlyUsagePeriod(now)
	counters, err := orgbilling.RecalculateOrgUsageCounters(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, orgID, periodStart, periodEnd)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo packages: package storage label recalc failed", "org_id", orgID, "error", err)
		return ""
	}
	return formatPackageBytes(counters.PackageStorageBytes)
}

func (h *Handlers) recalculatePackageUsage(r *http.Request, row reposdb.Repo) {
	if !row.OwnerOrgID.Valid {
		return
	}
	now := time.Now().UTC()
	periodStart, periodEnd := orgbilling.MonthlyUsagePeriod(now)
	if _, err := orgbilling.RecalculateOrgUsageCounters(r.Context(), orgbilling.Deps{Pool: h.d.Pool}, row.OwnerOrgID.Int64, periodStart, periodEnd); err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo packages: usage recalc failed", "repo_id", row.ID, "org_id", row.OwnerOrgID.Int64, "error", err)
	}
}

func packageNotice(v string) string {
	switch v {
	case "uploaded":
		return "Package published."
	case "deleted":
		return "Package deleted."
	default:
		return ""
	}
}

func packageInputMessage(err error) string {
	switch {
	case errors.Is(err, pkgdomain.ErrInvalidName):
		return "Package name must start with a letter or number and use only letters, numbers, dots, underscores, or hyphens."
	case errors.Is(err, pkgdomain.ErrInvalidVersion):
		return "Package version must start with a letter or number and use only letters, numbers, dots, underscores, plus signs, or hyphens."
	case errors.Is(err, pkgdomain.ErrInvalidFilename):
		return "Package file name is invalid."
	case errors.Is(err, pkgdomain.ErrInvalidFileSize):
		return "Package file exceeds the upload limit."
	case errors.Is(err, pkgdomain.ErrPackageFileExists):
		return "A file with that name already exists for this package version."
	default:
		return "Package upload failed."
	}
}

func cleanPackageFilename(name string) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	if name == "" {
		return ""
	}
	return path.Base(name)
}

func packageContentType(contentType, filename string) string {
	contentType = strings.TrimSpace(contentType)
	if contentType != "" {
		return contentType
	}
	if ext := filepath.Ext(filename); ext != "" {
		if detected := mime.TypeByExtension(ext); detected != "" {
			return detected
		}
	}
	return "application/octet-stream"
}

func formatPackageBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KB", "MB", "GB", "TB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PB", value/unit)
}
