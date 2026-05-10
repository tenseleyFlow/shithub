// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	mdrender "github.com/tenseleyFlow/shithub/internal/markdown"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	"github.com/tenseleyFlow/shithub/internal/repos/webedit"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

type codeEditorData struct {
	Title       string
	CSRFToken   string
	Owner       string
	Repo        any
	Ref         string
	RefDisplay  string
	BaseOID     string
	Path        string
	Crumbs      []Breadcrumb
	Mode        string
	FormAction  string
	CancelURL   string
	PreviewURL  string
	PathValue   string
	Content     string
	UploadDir   string
	Message     string
	Description string
	Primary     string
	Error       string
	Notice      string
	IsMarkdown  bool

	RepoCounts   any
	CanSettings  bool
	ActiveSubnav string
}

func (h *Handlers) codeEditForm(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.loadCodeContextFor(w, r, policy.ActionRepoWrite)
	if !ok || !h.requireEditableBranch(w, r, cc) {
		return
	}
	content, ok := h.editableBlobContent(w, r, cc)
	if !ok {
		return
	}
	data := h.editorData(r, cc, "edit", cc.subpath, string(content))
	h.renderEditor(w, r, data, http.StatusOK)
}

func (h *Handlers) codeEditSubmit(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.loadCodeContextFor(w, r, policy.ActionRepoWrite)
	if !ok || !h.requireEditableBranch(w, r, cc) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, webedit.MaxTextBytes+128*1024)
	if err := r.ParseForm(); err != nil {
		data := h.editorData(r, cc, "edit", cc.subpath, "")
		data.Error = "The submitted file is too large or could not be read."
		h.renderEditor(w, r, data, http.StatusRequestEntityTooLarge)
		return
	}
	target := cleanEditorPath(r.PostFormValue("path"))
	if target == "" {
		target = cc.subpath
	}
	content := []byte(r.PostFormValue("content"))
	if len(content) > webedit.MaxTextBytes {
		data := h.editorData(r, cc, "edit", target, string(content))
		data.Error = "Files edited in the browser must be 1 MiB or smaller."
		h.renderEditor(w, r, data, http.StatusRequestEntityTooLarge)
		return
	}
	if _, ok := h.editableBlobContent(w, r, cc); !ok {
		return
	}
	op := webedit.OpEdit
	if target != cc.subpath {
		op = webedit.OpRename
	}
	_, err := h.commitWebEdit(r, cc, webedit.Params{
		Op:          op,
		SourcePath:  cc.subpath,
		TargetPath:  target,
		Content:     content,
		BaseOID:     r.PostFormValue("base_oid"),
		Message:     submittedCommitMessage(r, op, cc, target, nil),
		Description: r.PostFormValue("commit_description"),
	})
	if err != nil {
		data := h.editorData(r, cc, "edit", target, string(content))
		data.Message = r.PostFormValue("commit_message")
		data.Description = r.PostFormValue("commit_description")
		h.renderWebEditError(w, r, data, err)
		return
	}
	http.Redirect(w, r, codeURL(cc.owner, cc.row.Name, "blob", cc.ref, target), http.StatusSeeOther)
}

func (h *Handlers) codeNewForm(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.loadCodeContextFor(w, r, policy.ActionRepoWrite)
	if !ok || !h.requireEditableBranch(w, r, cc) || !h.requireDirectory(w, r, cc) {
		return
	}
	prefix := cc.subpath
	if prefix != "" {
		prefix += "/"
	}
	data := h.editorData(r, cc, "new", prefix, "")
	h.renderEditor(w, r, data, http.StatusOK)
}

func (h *Handlers) codeNewSubmit(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.loadCodeContextFor(w, r, policy.ActionRepoWrite)
	if !ok || !h.requireEditableBranch(w, r, cc) || !h.requireDirectory(w, r, cc) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, webedit.MaxTextBytes+128*1024)
	if err := r.ParseForm(); err != nil {
		data := h.editorData(r, cc, "new", cc.subpath, "")
		data.Error = "The submitted file is too large or could not be read."
		h.renderEditor(w, r, data, http.StatusRequestEntityTooLarge)
		return
	}
	target := cleanEditorPath(r.PostFormValue("path"))
	content := []byte(r.PostFormValue("content"))
	if len(content) > webedit.MaxTextBytes {
		data := h.editorData(r, cc, "new", target, string(content))
		data.Error = "Files edited in the browser must be 1 MiB or smaller."
		h.renderEditor(w, r, data, http.StatusRequestEntityTooLarge)
		return
	}
	if _, err := h.commitWebEdit(r, cc, webedit.Params{
		Op:          webedit.OpCreate,
		TargetPath:  target,
		Content:     content,
		BaseOID:     r.PostFormValue("base_oid"),
		Message:     submittedCommitMessage(r, webedit.OpCreate, cc, target, nil),
		Description: r.PostFormValue("commit_description"),
	}); err != nil {
		data := h.editorData(r, cc, "new", target, string(content))
		data.Message = r.PostFormValue("commit_message")
		data.Description = r.PostFormValue("commit_description")
		h.renderWebEditError(w, r, data, err)
		return
	}
	http.Redirect(w, r, codeURL(cc.owner, cc.row.Name, "blob", cc.ref, target), http.StatusSeeOther)
}

func (h *Handlers) codeDeleteForm(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.loadCodeContextFor(w, r, policy.ActionRepoWrite)
	if !ok || !h.requireEditableBranch(w, r, cc) {
		return
	}
	if !h.deletableBlob(w, r, cc) {
		return
	}
	data := h.editorData(r, cc, "delete", cc.subpath, "")
	h.renderEditor(w, r, data, http.StatusOK)
}

func (h *Handlers) codeDeleteSubmit(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.loadCodeContextFor(w, r, policy.ActionRepoWrite)
	if !ok || !h.requireEditableBranch(w, r, cc) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128*1024)
	if err := r.ParseForm(); err != nil {
		data := h.editorData(r, cc, "delete", cc.subpath, "")
		data.Error = "The submitted form could not be read."
		h.renderEditor(w, r, data, http.StatusBadRequest)
		return
	}
	if _, err := h.commitWebEdit(r, cc, webedit.Params{
		Op:          webedit.OpDelete,
		SourcePath:  cc.subpath,
		BaseOID:     r.PostFormValue("base_oid"),
		Message:     submittedCommitMessage(r, webedit.OpDelete, cc, cc.subpath, nil),
		Description: r.PostFormValue("commit_description"),
	}); err != nil {
		data := h.editorData(r, cc, "delete", cc.subpath, "")
		data.Message = r.PostFormValue("commit_message")
		data.Description = r.PostFormValue("commit_description")
		h.renderWebEditError(w, r, data, err)
		return
	}
	http.Redirect(w, r, codeURL(cc.owner, cc.row.Name, "tree", cc.ref, parentPath(cc.subpath)), http.StatusSeeOther)
}

func (h *Handlers) codeUploadForm(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.loadCodeContextFor(w, r, policy.ActionRepoWrite)
	if !ok || !h.requireEditableBranch(w, r, cc) || !h.requireDirectory(w, r, cc) {
		return
	}
	data := h.editorData(r, cc, "upload", "", "")
	h.renderEditor(w, r, data, http.StatusOK)
}

func (h *Handlers) codeUploadSubmit(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.loadCodeContextFor(w, r, policy.ActionRepoWrite)
	if !ok || !h.requireEditableBranch(w, r, cc) || !h.requireDirectory(w, r, cc) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, webedit.MaxUploadBytes)
	if err := r.ParseMultipartForm(webedit.MaxUploadBytes); err != nil {
		data := h.editorData(r, cc, "upload", "", "")
		data.Error = "The uploaded files are too large or could not be read."
		h.renderEditor(w, r, data, http.StatusRequestEntityTooLarge)
		return
	}
	files, err := uploadedFiles(r, cc.subpath)
	if err != nil {
		data := h.editorData(r, cc, "upload", "", "")
		data.Message = r.PostFormValue("commit_message")
		data.Description = r.PostFormValue("commit_description")
		data.Error = friendlyWebEditError(err)
		h.renderEditor(w, r, data, editorStatus(err))
		return
	}
	if _, err := h.commitWebEdit(r, cc, webedit.Params{
		Op:          webedit.OpUpload,
		Files:       files,
		BaseOID:     r.PostFormValue("base_oid"),
		Message:     submittedCommitMessage(r, webedit.OpUpload, cc, "", files),
		Description: r.PostFormValue("commit_description"),
	}); err != nil {
		data := h.editorData(r, cc, "upload", "", "")
		data.Message = r.PostFormValue("commit_message")
		data.Description = r.PostFormValue("commit_description")
		h.renderWebEditError(w, r, data, err)
		return
	}
	target := codeURL(cc.owner, cc.row.Name, "tree", cc.ref, cc.subpath)
	if len(files) == 1 {
		target = codeURL(cc.owner, cc.row.Name, "blob", cc.ref, files[0].Path)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (h *Handlers) codeMarkdownPreview(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, webedit.MaxTextBytes+128*1024)
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusRequestEntityTooLarge, "")
		return
	}
	body := []byte(r.PostFormValue("content"))
	if len(body) > webedit.MaxTextBytes {
		h.d.Render.HTTPError(w, r, http.StatusRequestEntityTooLarge, "")
		return
	}
	rendered, err := mdrender.RenderDocumentHTML(body)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	ref := r.PostFormValue("ref")
	if ref == "" {
		ref = row.DefaultBranch
	}
	filePath := cleanEditorPath(r.PostFormValue("path"))
	dir := parentPath(filePath)
	if !validateSubpath(dir) {
		dir = ""
	}
	rendered = rewriteMarkdownRelativeURLs(
		rendered,
		codeRouteBase(owner.Username, row.Name, "blob", ref, dir),
		codeRouteBase(owner.Username, row.Name, "blob", ref, ""),
		codeRouteBase(owner.Username, row.Name, "raw", ref, dir),
		codeRouteBase(owner.Username, row.Name, "raw", ref, ""),
	)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.d.Render.RenderFragment(w, "repo/markdown_preview", map[string]any{
		"MarkdownHTML": template.HTML(rendered), //nolint:gosec // sanitized by markdown renderer
	}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "code: markdown preview", "error", err)
	}
}

func (h *Handlers) editorData(r *http.Request, cc *codeContext, mode, pathValue, content string) codeEditorData {
	head, headFound, _ := repogit.CommitAt(r.Context(), cc.gitDir, cc.ref)
	baseOID := ""
	if headFound {
		baseOID = head.OID
	}
	titleVerb := map[string]string{
		"edit":   "Edit",
		"new":    "Create new file",
		"delete": "Delete",
		"upload": "Upload files",
	}[mode]
	primary := map[string]string{
		"edit":   "Commit changes",
		"new":    "Commit new file",
		"delete": "Commit deletion",
		"upload": "Commit files",
	}[mode]
	if titleVerb == "" {
		titleVerb = "Edit"
	}
	cancelPath := cc.subpath
	cancelKind := "blob"
	if mode == "new" || mode == "upload" || cc.subpath == "" {
		cancelKind = "tree"
	}
	if mode == "delete" {
		cancelPath = cc.subpath
	}
	defaultOp := webedit.Op(mode)
	if mode == "edit" {
		defaultOp = webedit.OpEdit
	}
	if mode == "new" {
		defaultOp = webedit.OpCreate
	}
	if mode == "delete" {
		defaultOp = webedit.OpDelete
	}
	if mode == "upload" {
		defaultOp = webedit.OpUpload
	}
	message := webedit.DefaultMessage(defaultOp, cc.subpath, cleanEditorPath(pathValue), nil)
	if mode == "upload" {
		message = webedit.DefaultMessage(webedit.OpUpload, "", "", nil)
	}
	return codeEditorData{
		Title:        titleVerb + " · " + cc.row.Name,
		CSRFToken:    middleware.CSRFTokenForRequest(r),
		Owner:        cc.owner,
		Repo:         cc.row,
		Ref:          cc.ref,
		RefDisplay:   codeRefDisplay(cc.ref),
		BaseOID:      baseOID,
		Path:         cc.subpath,
		Crumbs:       breadcrumbs(cc.owner, cc.row.Name, cc.ref, cc.subpath),
		Mode:         mode,
		FormAction:   editorActionURL(cc.owner, cc.row.Name, mode, cc.ref, cc.subpath),
		CancelURL:    codeURL(cc.owner, cc.row.Name, cancelKind, cc.ref, cancelPath),
		PreviewURL:   "/" + cc.owner + "/" + cc.row.Name + "/markdown-preview",
		PathValue:    pathValue,
		Content:      content,
		UploadDir:    cc.subpath,
		Message:      message,
		Primary:      primary,
		IsMarkdown:   hasExt(strings.ToLower(pathValue), []string{".md", ".markdown"}),
		RepoCounts:   h.subnavCounts(r.Context(), cc.row.ID, cc.row.ForkCount),
		CanSettings:  h.canViewSettings(middleware.CurrentUserFromContext(r.Context())),
		ActiveSubnav: "code",
	}
}

func (h *Handlers) renderEditor(w http.ResponseWriter, r *http.Request, data codeEditorData, status int) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	h.d.Render.RenderPage(w, r, "repo/editor", data)
}

func (h *Handlers) renderWebEditError(w http.ResponseWriter, r *http.Request, data codeEditorData, err error) {
	if editorStatus(err) >= http.StatusInternalServerError {
		h.d.Logger.WarnContext(r.Context(), "code: web edit", "error", err)
	}
	data.Error = friendlyWebEditError(err)
	h.renderEditor(w, r, data, editorStatus(err))
}

func (h *Handlers) commitWebEdit(r *http.Request, cc *codeContext, p webedit.Params) (webedit.Result, error) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	p.GitDir = cc.gitDir
	p.Repo = cc.row
	p.Branch = cc.ref
	p.ActorUserID = viewer.ID
	p.RequestID = middleware.RequestIDFromContext(r.Context())
	return webedit.Commit(r.Context(), webedit.Deps{Pool: h.d.Pool, Logger: h.d.Logger}, p)
}

func (h *Handlers) requireEditableBranch(w http.ResponseWriter, r *http.Request, cc *codeContext) bool {
	if cc.isBranchRef() {
		return true
	}
	h.d.Render.HTTPError(w, r, http.StatusBadRequest, "Files can only be edited on a branch.")
	return false
}

func (h *Handlers) requireDirectory(w http.ResponseWriter, r *http.Request, cc *codeContext) bool {
	if cc.subpath == "" {
		return true
	}
	kind, _, _, err := repogit.StatPath(r.Context(), cc.gitDir, cc.ref, cc.subpath)
	if err != nil || kind != repogit.EntryTree {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return false
	}
	return true
}

func (h *Handlers) editableBlobContent(w http.ResponseWriter, r *http.Request, cc *codeContext) ([]byte, bool) {
	kind, _, size, err := repogit.StatPath(r.Context(), cc.gitDir, cc.ref, cc.subpath)
	if err != nil || kind != repogit.EntryBlob {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return nil, false
	}
	if size > webedit.MaxTextBytes {
		h.d.Render.HTTPError(w, r, http.StatusRequestEntityTooLarge, "Files edited in the browser must be 1 MiB or smaller.")
		return nil, false
	}
	body, err := repogit.ReadBlobBytes(r.Context(), cc.gitDir, cc.ref, cc.subpath, webedit.MaxTextBytes)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return nil, false
	}
	if webedit.IsBinary(body) {
		h.d.Render.HTTPError(w, r, http.StatusUnsupportedMediaType, "Binary files cannot be edited in the browser.")
		return nil, false
	}
	return body, true
}

func (h *Handlers) deletableBlob(w http.ResponseWriter, r *http.Request, cc *codeContext) bool {
	kind, _, _, err := repogit.StatPath(r.Context(), cc.gitDir, cc.ref, cc.subpath)
	if err != nil || kind != repogit.EntryBlob {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return false
	}
	return true
}

func uploadedFiles(r *http.Request, dir string) ([]webedit.File, error) {
	if r.MultipartForm == nil || r.MultipartForm.File == nil {
		return nil, webedit.ErrInvalidOperation
	}
	headers := r.MultipartForm.File["files"]
	if len(headers) == 0 {
		return nil, webedit.ErrInvalidOperation
	}
	files := make([]webedit.File, 0, len(headers))
	for _, header := range headers {
		name := strings.ReplaceAll(header.Filename, "\\", "/")
		name = path.Base(name)
		if name == "." || name == "/" || strings.TrimSpace(name) == "" {
			return nil, webedit.ErrInvalidPath
		}
		target := joinPath(dir, name)
		if err := webedit.ValidateFilePath(target); err != nil {
			return nil, err
		}
		f, err := header.Open()
		if err != nil {
			return nil, fmt.Errorf("webedit: open upload: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(f, webedit.MaxUploadFileBytes+1))
		closeErr := f.Close()
		if readErr != nil {
			return nil, fmt.Errorf("webedit: read upload: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("webedit: close upload: %w", closeErr)
		}
		if len(body) > webedit.MaxUploadFileBytes {
			return nil, webedit.ErrBlobTooLarge
		}
		files = append(files, webedit.File{Path: target, Body: body})
	}
	return files, nil
}

func friendlyWebEditError(err error) string {
	switch {
	case errors.Is(err, webedit.ErrNoVerifiedEmail):
		return "You need a verified primary email address before committing from the web editor."
	case errors.Is(err, webedit.ErrConflict):
		return "This branch changed while you were editing. Review your changes and try again."
	case errors.Is(err, webedit.ErrProtected):
		msg := strings.TrimPrefix(err.Error(), webedit.ErrProtected.Error()+": ")
		if msg != err.Error() {
			return msg
		}
		return "This branch is protected and cannot accept direct commits."
	case errors.Is(err, webedit.ErrPathExists):
		return "A file already exists at that path."
	case errors.Is(err, webedit.ErrPathNotFound):
		return "The file no longer exists on this branch."
	case errors.Is(err, webedit.ErrInvalidBranch):
		return "Choose a branch before committing changes."
	case errors.Is(err, webedit.ErrInvalidPath):
		return "Enter a valid repository path."
	case errors.Is(err, webedit.ErrUnsupportedEntry):
		return "Only regular files can be edited from the browser."
	case errors.Is(err, webedit.ErrBinary):
		return "Binary content cannot be edited from the browser."
	case errors.Is(err, webedit.ErrBlobTooLarge):
		return "The submitted file is too large."
	case errors.Is(err, webedit.ErrInvalidOperation):
		return "Choose at least one file to commit."
	default:
		return "The file could not be committed."
	}
}

func editorStatus(err error) int {
	switch {
	case errors.Is(err, webedit.ErrConflict), errors.Is(err, webedit.ErrPathExists):
		return http.StatusConflict
	case errors.Is(err, webedit.ErrProtected):
		return http.StatusForbidden
	case errors.Is(err, webedit.ErrPathNotFound):
		return http.StatusNotFound
	case errors.Is(err, webedit.ErrBlobTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, webedit.ErrInvalidPath), errors.Is(err, webedit.ErrInvalidBranch), errors.Is(err, webedit.ErrInvalidOperation), errors.Is(err, webedit.ErrUnsupportedEntry), errors.Is(err, webedit.ErrBinary), errors.Is(err, webedit.ErrNoVerifiedEmail):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func cleanEditorPath(p string) string {
	return strings.Trim(strings.TrimSpace(p), "/")
}

func parentPath(p string) string {
	if p == "" {
		return ""
	}
	parent := path.Dir(p)
	if parent == "." {
		return ""
	}
	return parent
}

func editorActionURL(owner, repoName, mode, ref, p string) string {
	return codeURL(owner, repoName, mode, ref, p)
}

func submittedCommitMessage(r *http.Request, op webedit.Op, cc *codeContext, target string, files []webedit.File) string {
	msg := strings.TrimSpace(r.PostFormValue("commit_message"))
	switch op {
	case webedit.OpRename:
		if msg == webedit.DefaultMessage(webedit.OpEdit, cc.subpath, cc.subpath, nil) {
			return ""
		}
	case webedit.OpCreate:
		if msg == "Create" || msg == webedit.DefaultMessage(webedit.OpCreate, "", cc.subpath, nil) || msg == webedit.DefaultMessage(webedit.OpCreate, "", cc.subpath+"/", nil) {
			return ""
		}
	case webedit.OpUpload:
		if msg == webedit.DefaultMessage(webedit.OpUpload, "", "", nil) {
			return ""
		}
	case webedit.OpDelete:
		if msg == "" {
			return ""
		}
	}
	return msg
}

func codeURL(owner, repoName, kind, ref, p string) string {
	base := "/" + owner + "/" + repoName + "/" + kind + "/" + ref
	if p == "" {
		return base + "/"
	}
	return base + "/" + p
}
