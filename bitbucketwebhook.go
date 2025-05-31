package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"go/ast"
	"go/parser"
	"go/token"
	"context"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/sheets/v4"
	"google.golang.org/api/option"
)

type PullRequestCreatedPayload struct {
	PullRequest struct {
		ID     int    `json:"id"`
		Title  string `json:"title"`
		Source struct {
			Branch struct {
				Name string `json:"name"`
			} `json:"branch"`
		} `json:"source"`
		Destination struct {
			Branch struct {
				Name string `json:"name"`
			} `json:"branch"`
		} `json:"destination"`
		Reviewers []struct {
			DisplayName string `json:"display_name"`
			UUID        string `json:"uuid"`
		} `json:"reviewers"`
		Description string `json:"description"`
	} `json:"pullrequest"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type FileContext struct {
	Path            string
	Content         string          // Complete file content
	PreviousContent string          // Content before changes
	Dependencies    map[string]bool // Map of imported packages
	TestContent     string          // Content of associated test file
	Language        string          // File language/type
}

type PRDescription struct {
	Summary     string
	FileChanges []string
	Impact      string
}

type CodeDefinition struct {
	Type       string // "struct", "interface", "function", "const", "var"
	Name       string
	Content    string
	FilePath   string
	References []string // List of other definitions this one references
}

type CommitInfo struct {
	Hash        string
	Author      string
	Date        string
	Message     string
	FilesChanged []string
}

type ArchitecturalContext struct {
	PackageDependencies []string
	ImportGraph        map[string][]string
	DatabaseSchemas    []string
	APIEndpoints      []string
	ConfigFiles       []string
}

type TestCase struct {
	ID          string
	Description string
	Steps       []string
	Expected    string
	Actual      string
	Status      string
}

type TestCaseContext struct {
	SheetURL     string
	SheetID      string
	TestCases    []TestCase
	TotalTests   int
	PassedTests  int
	FailedTests  int
	PendingTests int
}

type Inline struct {
	Path string `json:"path"`
	To   int    `json:"to,omitempty"`
	From int    `json:"from,omitempty"`
}

type Content struct {
	Raw string `json:"raw"`
}

type CommentPayload struct {
	Content Content `json:"content"`
	Inline  *Inline `json:"inline,omitempty"`
}

const chunkSeparator = "\n<<<<<<<<<<<< CHUNK SEPARATOR >>>>>>>>>>>\n"

const (
	endpoint   = "https://gpt3-5-sc.openai.azure.com"
	apiKey     = "your_api_token"
	apiVersion = "2024-12-01-preview"
	deployment = "gpt4Hackathon"
)

// GPTResponse represents the structure of the GPT-4 API response
type GPTResponse struct {
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// LanguageStats tracks the number of files per language
type LanguageStats struct {
	Language string
	Count    int
}

func runGitCommand(dir string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func sanitizeFilename(name string) string {
	// Replace characters that could cause issues in filenames
	replacer := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		":", "_",
		"*", "_",
		"?", "_",
		"\"", "_",
		"<", "_",
		">", "_",
		"|", "_",
		" ", "_",
	)
	return replacer.Replace(name)
}

func getFileContent(repoPath, filePath string) (string, error) {
	content, err := os.ReadFile(filepath.Join(repoPath, filePath))
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func getExactGitDiff(repoPath, sourceBranch, destBranch string) (string, error) {
	// Use git diff with full context and exact output
	cmd := exec.Command("git", "diff", "--full-index", "--no-color", 
		fmt.Sprintf("origin/%s...origin/%s", destBranch, sourceBranch))
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func getFileContext(repoPath, filePath string) (FileContext, error) {
	context := FileContext{
		Path:         filePath,
		Dependencies: make(map[string]bool),
		Language:     strings.TrimPrefix(filepath.Ext(filePath), "."),
	}

	// Get current file content
	content, err := getFileContent(repoPath, filePath)
	if err == nil {
		context.Content = content
	}

	// Get previous version of the file
	cmd := exec.Command("git", "show", fmt.Sprintf("HEAD:%s", filePath))
	cmd.Dir = repoPath
	prevContent, err := cmd.Output()
	if err == nil {
		context.PreviousContent = string(prevContent)
	}

	// Get associated test file content if it exists
	if strings.HasSuffix(filePath, ".go") {
		baseFile := strings.TrimSuffix(filepath.Base(filePath), ".go")
		testFile := filepath.Join(filepath.Dir(filePath), baseFile+"_test.go")
		testContent, err := getFileContent(repoPath, testFile)
		if err == nil {
			context.TestContent = testContent
		}
	}

	return context, nil
}

func gatherAllContext(repoPath string, changedFiles []string) (map[string]FileContext, error) {
	contexts := make(map[string]FileContext)
	
	for _, file := range changedFiles {
		context, err := getFileContext(repoPath, file)
		if err != nil {
			log.Printf("Warning: Error getting context for %s: %v", file, err)
			continue
		}
		contexts[file] = context
	}

	return contexts, nil
}

func getChangedFiles(repoPath, sourceBranch, destBranch string) ([]string, error) {
	cmd := exec.Command("git", "diff", "--name-only", fmt.Sprintf("origin/%s...origin/%s", destBranch, sourceBranch))
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSpace(string(output)), "\n"), nil
}

func generatePRDescription(repoPath string, changedFiles []string, diffOutput string) PRDescription {
	desc := PRDescription{
		FileChanges: make([]string, 0),
	}

	// Get file type changes
	fileTypes := make(map[string]int)
	for _, file := range changedFiles {
		ext := filepath.Ext(file)
		if ext == "" {
			ext = "other"
		}
		fileTypes[ext]++
		desc.FileChanges = append(desc.FileChanges, fmt.Sprintf("- %s", file))
	}

	// Generate summary based on file types
	var summary strings.Builder
	summary.WriteString("This PR includes changes to ")
	if len(fileTypes) == 0 {
		summary.WriteString("configuration or documentation")
	} else {
		i := 0
		for ext, count := range fileTypes {
			if i > 0 {
				if i == len(fileTypes)-1 {
					summary.WriteString(" and ")
				} else {
					summary.WriteString(", ")
				}
			}
			fileType := strings.TrimPrefix(ext, ".")
			if fileType == "" {
				fileType = "other"
			}
			summary.WriteString(fmt.Sprintf("%d %s files", count, fileType))
			i++
		}
	}
	summary.WriteString(".")
	desc.Summary = summary.String()

	// Determine potential impact
	var impact strings.Builder
	impact.WriteString("### Impact Assessment\n")
	
	// Check for specific file patterns
	hasTests := false
	hasConfig := false
	hasDocs := false
	hasCore := false

	for _, file := range changedFiles {
		switch {
		case strings.Contains(file, "_test."):
			hasTests = true
		case strings.Contains(file, "config"):
			hasConfig = true
		case strings.Contains(file, "docs") || strings.HasSuffix(file, ".md"):
			hasDocs = true
		case strings.HasSuffix(file, ".go") || strings.HasSuffix(file, ".js") || strings.HasSuffix(file, ".py"):
			hasCore = true
		}
	}

	if hasCore {
		impact.WriteString("- 🔄 Core functionality changes\n")
	}
	if hasConfig {
		impact.WriteString("- ⚙️ Configuration changes\n")
	}
	if hasTests {
		impact.WriteString("- ✅ Test coverage changes\n")
	}
	if hasDocs {
		impact.WriteString("- 📚 Documentation updates\n")
	}

	desc.Impact = impact.String()
	return desc
}

func formatPRDescription(desc PRDescription) string {
	var builder strings.Builder

	builder.WriteString("## Description\n")
	builder.WriteString(desc.Summary)
	builder.WriteString("\n\n")

	builder.WriteString("### Changed Files\n")
	for _, change := range desc.FileChanges {
		builder.WriteString(change)
		builder.WriteString("\n")
	}
	builder.WriteString("\n")

	builder.WriteString(desc.Impact)
	builder.WriteString("\n")

	return builder.String()
}

func findDefinitionInFile(filePath string, identifier string) (CodeDefinition, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return CodeDefinition{}, err
	}

	fileContent := string(content)
	lines := strings.Split(fileContent, "\n")

	for i, line := range lines {
		if strings.Contains(line, identifier) {
			// Check for struct definitions
			if strings.Contains(line, "type "+identifier+" struct") {
				def := CodeDefinition{
					Type:     "struct",
					Name:     identifier,
					FilePath: filePath,
				}
				// Capture the entire struct definition
				structContent := []string{line}
				braceCount := strings.Count(line, "{") - strings.Count(line, "}")
				for j := i + 1; j < len(lines) && braceCount > 0; j++ {
					structContent = append(structContent, lines[j])
					braceCount += strings.Count(lines[j], "{") - strings.Count(lines[j], "}")
				}
				def.Content = strings.Join(structContent, "\n")
				return def, nil
			}

			// Check for function definitions
			if strings.Contains(line, "func") && (strings.Contains(line, " "+identifier+"(") || strings.Contains(line, " (*"+identifier+")")) {
				def := CodeDefinition{
					Type:     "function",
					Name:     identifier,
					FilePath: filePath,
				}
				// Capture the entire function definition
				funcContent := []string{line}
				if !strings.Contains(line, "{") {
					continue
				}
				braceCount := strings.Count(line, "{") - strings.Count(line, "}")
				for j := i + 1; j < len(lines) && braceCount > 0; j++ {
					funcContent = append(funcContent, lines[j])
					braceCount += strings.Count(lines[j], "{") - strings.Count(lines[j], "}")
				}
				def.Content = strings.Join(funcContent, "\n")
				return def, nil
			}
		}
	}
	return CodeDefinition{}, fmt.Errorf("definition not found")
}

func findReferencedDefinitions(repoPath string, diffOutput string) ([]CodeDefinition, error) {
	var definitions []CodeDefinition
	seenDefinitions := make(map[string]bool)

	// Regular expressions to find potential identifiers
	typeRegex := regexp.MustCompile(`type\s+(\w+)\s+struct`)
	funcRegex := regexp.MustCompile(`func\s+(\w+)\s*\(`)
	methodRegex := regexp.MustCompile(`func\s+\(\w+\s+\*?(\w+)\)\s+\w+\(`)
	varRegex := regexp.MustCompile(`var\s+(\w+)\s+\w+`)
	
	// Find all .go files in the repository
	var goFiles []string
	err := filepath.Walk(repoPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".go") {
			goFiles = append(goFiles, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Extract identifiers from diff
	matches := map[string]bool{}
	
	// Find type definitions
	for _, match := range typeRegex.FindAllStringSubmatch(diffOutput, -1) {
		matches[match[1]] = true
	}
	
	// Find function definitions
	for _, match := range funcRegex.FindAllStringSubmatch(diffOutput, -1) {
		matches[match[1]] = true
	}
	
	// Find method definitions
	for _, match := range methodRegex.FindAllStringSubmatch(diffOutput, -1) {
		matches[match[1]] = true // Add the type name
	}

	// Find variable definitions
	for _, match := range varRegex.FindAllStringSubmatch(diffOutput, -1) {
		matches[match[1]] = true
	}

	// Find definitions in all Go files
	for identifier := range matches {
		if seenDefinitions[identifier] {
			continue
		}

		for _, file := range goFiles {
			if def, err := findDefinitionInFile(file, identifier); err == nil {
				definitions = append(definitions, def)
				seenDefinitions[identifier] = true
				break
			}
		}
	}

	return definitions, nil
}

func formatDefinitions(definitions []CodeDefinition) string {
	if len(definitions) == 0 {
		return "No additional definitions found"
	}

	var builder strings.Builder
	builder.WriteString("### Related Code Definitions\n\n")

	// Group definitions by type
	structDefs := []CodeDefinition{}
	funcDefs := []CodeDefinition{}
	otherDefs := []CodeDefinition{}

	for _, def := range definitions {
		switch def.Type {
		case "struct":
			structDefs = append(structDefs, def)
		case "function":
			funcDefs = append(funcDefs, def)
		default:
			otherDefs = append(otherDefs, def)
		}
	}

	// Format structs
	if len(structDefs) > 0 {
		builder.WriteString("#### Type Definitions\n")
		for _, def := range structDefs {
			builder.WriteString(fmt.Sprintf("\nFrom `%s`:\n", def.FilePath))
			builder.WriteString("```go\n")
			builder.WriteString(def.Content)
			builder.WriteString("\n```\n")
		}
	}

	// Format functions
	if len(funcDefs) > 0 {
		builder.WriteString("\n#### Function Definitions\n")
		for _, def := range funcDefs {
			builder.WriteString(fmt.Sprintf("\nFrom `%s`:\n", def.FilePath))
			builder.WriteString("```go\n")
			builder.WriteString(def.Content)
			builder.WriteString("\n```\n")
		}
	}

	// Format others
	if len(otherDefs) > 0 {
		builder.WriteString("\n#### Other Definitions\n")
		for _, def := range otherDefs {
			builder.WriteString(fmt.Sprintf("\nFrom `%s`:\n", def.FilePath))
			builder.WriteString("```go\n")
			builder.WriteString(def.Content)
			builder.WriteString("\n```\n")
		}
	}

	return builder.String()
}

// detectRepoLanguages analyzes the repository to determine primary languages
func detectRepoLanguages(repoPath string) ([]LanguageStats, error) {
	languageCounts := make(map[string]int)

	err := filepath.Walk(repoPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Skip common non-code directories
			base := filepath.Base(path)
			if base == "node_modules" || base == "vendor" || base == ".git" {
				return filepath.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".go":
			languageCounts["Go"]++
		case ".js", ".jsx", ".ts", ".tsx":
			languageCounts["JavaScript/TypeScript"]++
		case ".py":
			languageCounts["Python"]++
		case ".java":
			languageCounts["Java"]++
		case ".rb":
			languageCounts["Ruby"]++
		case ".php":
			languageCounts["PHP"]++
		case ".cs":
			languageCounts["C#"]++
		case ".cpp", ".cc", ".cxx", ".hpp":
			languageCounts["C++"]++
		case ".c", ".h":
			languageCounts["C"]++
		case ".rs":
			languageCounts["Rust"]++
		case ".swift":
			languageCounts["Swift"]++
		case ".kt":
			languageCounts["Kotlin"]++
		case ".scala":
			languageCounts["Scala"]++
		}
		return nil
	})

	if err != nil {
		return nil, err
	}

	// Convert map to slice and sort by count
	var stats []LanguageStats
	for lang, count := range languageCounts {
		stats = append(stats, LanguageStats{Language: lang, Count: count})
	}

	// Sort by count in descending order
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].Count > stats[j].Count
	})

	return stats, nil
}

func generateMetadataChunk(payload PullRequestCreatedPayload, fullRepo, sourceBranch, destBranch string, changedFiles []string, repoPath string) string {
	// Construct the repository path
	repoName := strings.ReplaceAll(strings.ReplaceAll(fullRepo, "/", "_"), ".", "_")
	repoPath = filepath.Join("/Users/abhyuday.tomar/exotel/hackathon/repos", repoName)

	// Detect repository languages
	languages, err := detectRepoLanguages(repoPath)
	languageInfo := "Unable to detect repository languages"
	if err == nil && len(languages) > 0 {
		var langStrings []string
		for _, lang := range languages {
			langStrings = append(langStrings, fmt.Sprintf("%s (%d files)", lang.Language, lang.Count))
		}
		languageInfo = strings.Join(langStrings, ", ")
	}

	return fmt.Sprintf(`### CHUNK: PR METADATA
# Pull Request Overview
- PR ID: %d
- Title: %s
- Repository: %s
- Repository URL: https://bitbucket.org/%s
- Source Branch: %s
- Target Branch: %s
- Created/Updated At: %s
- Repository Languages: %s

## Reviewers
%s

## Files Changed
Total files changed: %d
`,
		payload.PullRequest.ID,
		payload.PullRequest.Title,
		fullRepo,
		fullRepo,
		sourceBranch,
		destBranch,
		time.Now().Format("2006-01-02 15:04:05"),
		languageInfo,
		formatReviewers(payload.PullRequest.Reviewers),
		len(changedFiles))
}

func generateDescriptionChunk(prDesc PRDescription) string {
	return fmt.Sprintf(`### CHUNK: PR DESCRIPTION
# Change Description
%s`, formatPRDescription(prDesc))
}

func generateContextChunk(definitions []CodeDefinition) string {
	return fmt.Sprintf(`### CHUNK: CODE CONTEXT
# Related Code Definitions
%s`, formatDefinitions(definitions))
}

func generateDiffChunk(diffOutput string) string {
	return fmt.Sprintf(`### CHUNK: GIT DIFF
# Changes Made
` + "```" + `diff
%s
` + "```", diffOutput)
}

func generateFileContentsChunk(contexts map[string]FileContext) string {
	return fmt.Sprintf(`### CHUNK: COMPLETE FILES
# Complete File Contents
%s`, formatCompleteFileContents(contexts))
}

func generateReviewInstructionsChunk() string {
	return `### CHUNK: REVIEW INSTRUCTIONS
# Code Review Guidelines

You are a highly experienced software engineer and professional code reviewer. Your role is to carefully analyze submitted code changes as if you are reviewing them for a critical production system. You take your responsibility seriously — providing detailed, thoughtful, and actionable feedback to ensure the code is correct, maintainable, secure, and high quality. You strive to communicate clearly and constructively, helping the author improve their work effectively.

## Primary Focus
1. Code Correctness
   - Logic errors
   - Edge cases
   - Error handling
   - Race conditions

2. Code Quality
   - Best practices
   - Design patterns
   - Code organization
   - Naming conventions

3. Performance
   - Time complexity
   - Space complexity
   - Resource usage
   - Bottlenecks

4. Security
   - Input validation
   - Authentication/Authorization
   - Data protection
   - Security best practices

## Secondary Focus

1.Maintainability
   - Code duplication
   - Modularity
   - Extensibility

2. Breaking Changes
   - API compatibility
   - Database schema changes
   - Configuration changes

Please provide specific, actionable feedback for each issue found, including:
- Issue description
- Impact assessment
- Suggested improvements
- Code examples where applicable`
}

func generateReviewOutputFormatChunk() string {
	return `### CHUNK: REVIEW OUTPUT FORMAT
# Review Output Format

The code review comments must be provided in the following JSON format:

[
  {
    "inline": {
      "path": "path/to/file",
      "to": lineNumber
    },
    "content": {
      "raw": "The review comment text"
    }
  }
]

## Format Rules:
1. Each comment must be an object in the array
2. For inline comments:
   - Include the "inline" object with file path and line number
   - The "to" field specifies the line number the comment refers to
3. For general comments:
   - Omit the "inline" object
   - Only include the "content" object
4. The "raw" field contains the actual review comment text`
}

func getRecentCommits(repoPath, filePath string, numCommits int) ([]CommitInfo, error) {
	var commits []CommitInfo
	
	cmd := exec.Command("git", "log", "-n", fmt.Sprintf("%d", numCommits), "--pretty=format:%H|%an|%ad|%s", "--date=short", "--", filePath)
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return commits, err
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) >= 4 {
			commit := CommitInfo{
				Hash:    parts[0],
				Author:  parts[1],
				Date:    parts[2],
				Message: parts[3],
			}
			
			// Get files changed in this commit
			filesCmd := exec.Command("git", "show", "--name-only", "--pretty=format:", commit.Hash)
			filesCmd.Dir = repoPath
			if filesOutput, err := filesCmd.Output(); err == nil {
				commit.FilesChanged = strings.Split(strings.TrimSpace(string(filesOutput)), "\n")
			}
			
			commits = append(commits, commit)
		}
	}
	return commits, nil
}

func getArchitecturalContext(repoPath string) (ArchitecturalContext, error) {
	context := ArchitecturalContext{
		ImportGraph: make(map[string][]string),
	}
	
	// Find all Go files
	var goFiles []string
	err := filepath.Walk(repoPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			switch {
			case strings.HasSuffix(path, ".go"):
				goFiles = append(goFiles, path)
			case strings.HasSuffix(path, ".sql"):
				context.DatabaseSchemas = append(context.DatabaseSchemas, path)
			case strings.Contains(path, "config") || strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".json"):
				context.ConfigFiles = append(context.ConfigFiles, path)
			}
		}
		return nil
	})
	if err != nil {
		return context, err
	}

	// Analyze imports and API endpoints
	for _, file := range goFiles {
		content, err := os.ReadFile(file)
		if err != nil {
			continue
		}

		// Parse file for imports
		fset := token.NewFileSet()
		if f, err := parser.ParseFile(fset, file, content, parser.ParseComments); err == nil {
			var imports []string
			for _, imp := range f.Imports {
				importPath := strings.Trim(imp.Path.Value, "\"")
				imports = append(imports, importPath)
				if !contains(context.PackageDependencies, importPath) {
					context.PackageDependencies = append(context.PackageDependencies, importPath)
				}
			}
			context.ImportGraph[file] = imports

			// Look for HTTP handlers and API endpoints
			ast.Inspect(f, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
						if sel.Sel.Name == "HandleFunc" || sel.Sel.Name == "Handle" {
							if len(call.Args) > 0 {
								if lit, ok := call.Args[0].(*ast.BasicLit); ok {
									endpoint := strings.Trim(lit.Value, "\"")
									context.APIEndpoints = append(context.APIEndpoints, endpoint)
								}
							}
						}
					}
				}
				return true
			})
		}
	}

	return context, nil
}

func generateCommitHistoryChunk(repoPath string, changedFiles []string) string {
	var builder strings.Builder
	builder.WriteString("### CHUNK: COMMIT HISTORY\n")
	builder.WriteString("# Recent Changes History\n\n")

	for _, file := range changedFiles {
		commits, err := getRecentCommits(repoPath, file, 5)
		if err != nil {
			continue
		}

		builder.WriteString(fmt.Sprintf("## File: %s\n", file))
		builder.WriteString("Recent commits:\n")
		for _, commit := range commits {
			builder.WriteString(fmt.Sprintf("* %s (%s) by %s\n", 
				commit.Message,
				commit.Date,
				commit.Author))
			builder.WriteString("  Changed files:\n")
			for _, f := range commit.FilesChanged {
				builder.WriteString(fmt.Sprintf("  - %s\n", f))
			}
			builder.WriteString("\n")
		}
		builder.WriteString("\n")
	}

	return builder.String()
}

func generateArchitecturalChunk(repoPath string) string {
	context, err := getArchitecturalContext(repoPath)
	if err != nil {
		return "Error gathering architectural context"
	}

	var builder strings.Builder
	builder.WriteString("### CHUNK: ARCHITECTURAL CONTEXT\n")
	builder.WriteString("# System Architecture Overview\n\n")

	// Package Dependencies
	builder.WriteString("## Package Dependencies\n")
	for _, dep := range context.PackageDependencies {
		builder.WriteString(fmt.Sprintf("- %s\n", dep))
	}
	builder.WriteString("\n")

	// API Endpoints
	builder.WriteString("## API Endpoints\n")
	for _, endpoint := range context.APIEndpoints {
		builder.WriteString(fmt.Sprintf("- %s\n", endpoint))
	}
	builder.WriteString("\n")

	// Config Files
	builder.WriteString("## Configuration Files\n")
	for _, config := range context.ConfigFiles {
		builder.WriteString(fmt.Sprintf("- %s\n", config))
	}
	builder.WriteString("\n")

	// Database Schemas
	if len(context.DatabaseSchemas) > 0 {
		builder.WriteString("## Database Schema Files\n")
		for _, schema := range context.DatabaseSchemas {
			builder.WriteString(fmt.Sprintf("- %s\n", schema))
		}
	}

	return builder.String()
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func extractGoogleSheetURL(description string) string {
	// Regular expression to match Google Sheets URLs
	re := regexp.MustCompile(`https://docs\.google\.com/spreadsheets/d/([a-zA-Z0-9-_]+)(/edit[#?].*)?`)
	match := re.FindStringSubmatch(description)
	if len(match) > 1 {
		return match[0]
	}
	return ""
}

func getSheetIDFromURL(url string) string {
	re := regexp.MustCompile(`/d/([a-zA-Z0-9-_]+)`)
	match := re.FindStringSubmatch(url)
	if len(match) > 1 {
		return match[1]
	}
	return ""
}

func getTestCasesFromSheet(sheetURL string) (TestCaseContext, error) {
	ctx := context.Background()
	testContext := TestCaseContext{
		SheetURL: sheetURL,
		SheetID:  getSheetIDFromURL(sheetURL),
	}

	if testContext.SheetID == "" {
		return testContext, fmt.Errorf("invalid sheet URL")
	}

	// Load Google Sheets credentials from environment
	credentials := os.Getenv("GOOGLE_SHEETS_CREDENTIALS")
	if credentials == "" {
		return testContext, fmt.Errorf("GOOGLE_SHEETS_CREDENTIALS environment variable not set")
	}

	config, err := google.JWTConfigFromJSON([]byte(credentials), sheets.SpreadsheetsReadonlyScope)
	if err != nil {
		return testContext, fmt.Errorf("unable to parse credentials: %v", err)
	}

	client := config.Client(ctx)
	srv, err := sheets.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return testContext, fmt.Errorf("unable to create sheets service: %v", err)
	}

	// Assuming test cases are in the first sheet
	resp, err := srv.Spreadsheets.Values.Get(testContext.SheetID, "A2:F").Do()
	if err != nil {
		return testContext, fmt.Errorf("unable to retrieve data from sheet: %v", err)
	}

	if len(resp.Values) == 0 {
		return testContext, fmt.Errorf("no data found in sheet")
	}

	// Process test cases
	for _, row := range resp.Values {
		if len(row) < 6 {
			continue
		}

		testCase := TestCase{
			ID:          fmt.Sprintf("%v", row[0]),
			Description: fmt.Sprintf("%v", row[1]),
			Steps:       strings.Split(fmt.Sprintf("%v", row[2]), "\n"),
			Expected:    fmt.Sprintf("%v", row[3]),
			Actual:      fmt.Sprintf("%v", row[4]),
			Status:      fmt.Sprintf("%v", row[5]),
		}

		testContext.TestCases = append(testContext.TestCases, testCase)

		// Update statistics
		testContext.TotalTests++
		switch strings.ToLower(testCase.Status) {
		case "pass", "passed":
			testContext.PassedTests++
		case "fail", "failed":
			testContext.FailedTests++
		default:
			testContext.PendingTests++
		}
	}

	return testContext, nil
}

func generateTestCaseChunk(testContext TestCaseContext) string {
	if testContext.TotalTests == 0 {
		return "### CHUNK: TEST CASES\n# No test cases found\n"
	}

	var builder strings.Builder
	builder.WriteString("### CHUNK: TEST CASES\n")
	builder.WriteString("# Test Case Summary\n\n")

	// Add test statistics
	builder.WriteString(fmt.Sprintf("## Test Statistics\n"))
	builder.WriteString(fmt.Sprintf("- Total Test Cases: %d\n", testContext.TotalTests))
	builder.WriteString(fmt.Sprintf("- Passed: %d (%.1f%%)\n", 
		testContext.PassedTests, 
		float64(testContext.PassedTests)/float64(testContext.TotalTests)*100))
	builder.WriteString(fmt.Sprintf("- Failed: %d (%.1f%%)\n", 
		testContext.FailedTests,
		float64(testContext.FailedTests)/float64(testContext.TotalTests)*100))
	builder.WriteString(fmt.Sprintf("- Pending: %d (%.1f%%)\n\n",
		testContext.PendingTests,
		float64(testContext.PendingTests)/float64(testContext.TotalTests)*100))

	builder.WriteString("## Test Cases\n")
	for _, test := range testContext.TestCases {
		builder.WriteString(fmt.Sprintf("\n### Test Case %s\n", test.ID))
		builder.WriteString(fmt.Sprintf("**Description:** %s\n\n", test.Description))
		
		builder.WriteString("**Steps:**\n")
		for i, step := range test.Steps {
			builder.WriteString(fmt.Sprintf("%d. %s\n", i+1, step))
		}
		
		builder.WriteString(fmt.Sprintf("\n**Expected Result:** %s\n", test.Expected))
		builder.WriteString(fmt.Sprintf("**Actual Result:** %s\n", test.Actual))
		builder.WriteString(fmt.Sprintf("**Status:** %s\n", test.Status))
	}

	builder.WriteString(fmt.Sprintf("\nTest Cases Sheet: %s\n", testContext.SheetURL))
	return builder.String()
}

func writeDiffToFile(diffOutput, fullRepo, sourceBranch, destBranch string, payload PullRequestCreatedPayload, repoPath string) error {
	timestamp := time.Now().Format("2006-01-02_15-04-05")
	outputDir := "./diffs"
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %v", err)
	}

	// Get exact git diff
	exactDiff, err := getExactGitDiff(repoPath, sourceBranch, destBranch)
	if err != nil {
		log.Printf("Warning: Error getting exact diff: %v", err)
		exactDiff = diffOutput
	}

	// Find related code definitions
	definitions, err := findReferencedDefinitions(repoPath, exactDiff)
	if err != nil {
		log.Printf("Warning: Error finding related definitions: %v", err)
	}

	// Get changed files
	changedFiles, err := getChangedFiles(repoPath, sourceBranch, destBranch)
	if err != nil {
		log.Printf("Warning: Error getting changed files: %v", err)
		changedFiles = []string{}
	}

	// Generate PR description
	prDesc := generatePRDescription(repoPath, changedFiles, exactDiff)

	fileContexts, err := gatherAllContext(repoPath, changedFiles)
	if err != nil {
		log.Printf("Warning: Error gathering context: %v", err)
	}

	// Sanitize filename components
	safeRepo := sanitizeFilename(fullRepo)
	safeSrcBranch := sanitizeFilename(sourceBranch)
	safeDestBranch := sanitizeFilename(destBranch)

	filename := filepath.Join(outputDir, fmt.Sprintf("%s_%s_to_%s_%s.txt",
		safeRepo,
		safeSrcBranch,
		safeDestBranch,
		timestamp))

	// Extract and fetch test cases if available
	var testCaseChunk string
	if sheetURL := extractGoogleSheetURL(payload.PullRequest.Title + "\n" + payload.PullRequest.Description); sheetURL != "" {
		if testContext, err := getTestCasesFromSheet(sheetURL); err == nil {
			testCaseChunk = generateTestCaseChunk(testContext)
		} else {
			log.Printf("Warning: Error fetching test cases: %v", err)
			testCaseChunk = "### CHUNK: TEST CASES\n# Error fetching test cases\n" + err.Error()
		}
	} else {
		testCaseChunk = "### CHUNK: TEST CASES\n# No test cases sheet found in PR description\n"
	}

	// Generate chunks
	chunks := []string{
		generateMetadataChunk(payload, fullRepo, sourceBranch, destBranch, changedFiles, repoPath),
		generateDescriptionChunk(prDesc),
		generateArchitecturalChunk(repoPath),
		generateCommitHistoryChunk(repoPath, changedFiles),
		testCaseChunk,
		generateContextChunk(definitions),
		generateDiffChunk(exactDiff),
		generateFileContentsChunk(fileContexts),
		generateReviewInstructionsChunk(),
		generateReviewOutputFormatChunk(),
	}

	// Update the guide
	guide := `# Code Review Chunks Guide
This file is organized into separate chunks for staged review:

1. PR METADATA - Basic information about the pull request and repository languages
2. PR DESCRIPTION - Detailed description of changes
3. ARCHITECTURAL CONTEXT - System architecture and dependencies
4. COMMIT HISTORY - Recent changes to affected files
5. TEST CASES - Test cases and execution status
6. CODE CONTEXT - Related code definitions and dependencies
7. GIT DIFF - Actual changes made
8. COMPLETE FILES - Full content of changed files
9. REVIEW INSTRUCTIONS - Guidelines for code review
10. REVIEW OUTPUT FORMAT - Expected format for review comments

Each chunk is separated by: ` + chunkSeparator + "\n\n"

	content := guide + strings.Join(chunks, chunkSeparator)

	err = os.WriteFile(filename, []byte(content), 0644)
	if err != nil {
		return fmt.Errorf("failed to write diff file: %v", err)
	}

	log.Printf("AI-ready code review diff written to file: %s", filename)
	return nil
}

func formatReviewers(reviewers []struct {
	DisplayName string `json:"display_name"`
	UUID        string `json:"uuid"`
}) string {
	if len(reviewers) == 0 {
		return "No reviewers assigned"
	}

	var reviewerList strings.Builder
	for i, reviewer := range reviewers {
		reviewerList.WriteString(fmt.Sprintf("%d. %s\n", i+1, reviewer.DisplayName))
	}
	return reviewerList.String()
}

func formatCompleteFileContents(contexts map[string]FileContext) string {
	if len(contexts) == 0 {
		return "No file contents available"
	}

	var builder strings.Builder
	
	for path, ctx := range contexts {
		builder.WriteString(fmt.Sprintf("\n### %s\n", path))
		
		// Show current content with exact formatting
		builder.WriteString("```")
		if ctx.Language != "" {
			builder.WriteString(ctx.Language)
		}
		builder.WriteString("\n")
		builder.WriteString(ctx.Content)
		if !strings.HasSuffix(ctx.Content, "\n") {
			builder.WriteString("\n")
		}
		builder.WriteString("```\n")

		// Show test file if it exists
		if ctx.TestContent != "" {
			builder.WriteString(fmt.Sprintf("\n### Test File: %s\n", path+"_test."+ctx.Language))
			builder.WriteString("```")
			if ctx.Language != "" {
				builder.WriteString(ctx.Language)
			}
			builder.WriteString("\n")
			builder.WriteString(ctx.TestContent)
			if !strings.HasSuffix(ctx.TestContent, "\n") {
				builder.WriteString("\n")
			}
			builder.WriteString("```\n")
		}
	}
	
	return builder.String()
}

func fetchAndDiff(fullRepo, sourceBranch, destBranch string, payload PullRequestCreatedPayload) {
	username := "codecoolexotel-admin"
	appPassword := "ATBB4hcbBaG6Y5VypBVYfgm9X7rPCFAD2D09"

	cloneURL := fmt.Sprintf("https://%s:%s@bitbucket.org/%s.git", username, appPassword, fullRepo)

	baseRepoDir := "/Users/abhyuday.tomar/exotel/hackathon/repos"
	repoName := strings.ReplaceAll(strings.ReplaceAll(fullRepo, "/", "_"), ".", "_")
	cloneDir := filepath.Join(baseRepoDir, repoName)

	// Clone or pull repo
	if _, err := os.Stat(cloneDir); os.IsNotExist(err) {
		log.Printf("Cloning repo to: %s", cloneDir)
		if output, err := runGitCommand("", "git", "clone", cloneURL, cloneDir); err != nil {
			log.Printf("Clone failed: %v\n%s", err, output)
			return
		}
	} else {
		log.Printf("Repo already exists at %s. Pulling latest changes...", cloneDir)
		if output, err := runGitCommand(cloneDir, "git", "pull"); err != nil {
			log.Printf("Git pull failed: %v\n%s", err, output)
			return
		}
	}

	// Fetch both branches
	for _, branch := range []string{sourceBranch, destBranch} {
		if output, err := runGitCommand(cloneDir, "git", "fetch", "origin", branch); err != nil {
			log.Printf("Failed to fetch branch %s: %v\n%s", branch, err, output)
			return
		}
	}

	// Diff
	log.Printf("Getting diff between '%s' and '%s'...", destBranch, sourceBranch)
	diffOutput, err := runGitCommand(cloneDir, "git", "diff", fmt.Sprintf("origin/%s", destBranch), fmt.Sprintf("origin/%s", sourceBranch))
	if err != nil {
		log.Printf("Diff command failed: %v\n%s", err, diffOutput)
		return
	}

	if strings.TrimSpace(diffOutput) == "" {
		log.Println("No differences found between branches.")
	} else {
		// Write diff to file with full PR context
		if err := writeDiffToFile(diffOutput, fullRepo, sourceBranch, destBranch, payload, cloneDir); err != nil {
			log.Printf("Error writing diff to file: %v", err)
		}
	}
}

func basicAuth(username, password string) string {
	auth := username + ":" + password
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(auth))
}

func postComment(comment CommentPayload, payload PullRequestCreatedPayload) error {
	username := "codecoolexotel-admin"
	appPassword := "ATBB4hcbBaG6Y5VypBVYfgm9X7rPCFAD2D09"

	url := fmt.Sprintf("https://api.bitbucket.org/2.0/repositories/%s/pullrequests/%d/comments", payload.Repository.FullName, payload.PullRequest.ID)

	log.Printf("url to post comment: %v", url)

	body, err := json.Marshal(comment)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", basicAuth(username, appPassword))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("Bitbucket API returned status %d", resp.StatusCode)
	}

	log.Println("Comment posted successfully")
	return nil
}

// analyzeWithGPT4 sends the PR content to GPT-4 for analysis and returns the response
func analyzeWithGPT4(prompt string) (string, error) {
	url := fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s", endpoint, deployment, apiVersion)

	requestBody := map[string]interface{}{
		"messages": []map[string]string{
			{
				"role":    "user",
				"content": prompt,
			},
		},
		"temperature": 0.7,
		"max_tokens":  1000,
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", apiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	// Log raw response for debugging
	log.Printf("Raw GPT-4 Response:\n%s\n", string(bodyBytes))

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API error: %s - %s", resp.Status, string(bodyBytes))
	}

	var response GPTResponse
	if err := json.Unmarshal(bodyBytes, &response); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if len(response.Choices) == 0 {
		return "", fmt.Errorf("no response choices returned")
	}

	return response.Choices[0].Message.Content, nil
}

// parseGPT4Response converts GPT-4's response into CommentPayload structs
func parseGPT4Response(response string) ([]CommentPayload, error) {
	var comments []CommentPayload
	
	// First try direct JSON parsing
	if err := json.Unmarshal([]byte(response), &comments); err == nil {
		return comments, nil
	}

	// If direct parsing fails, try to extract JSON from the response
	// Look for content between triple backticks
	jsonPattern := "```json\\s*([\\s\\S]*?)```"
	re := regexp.MustCompile(jsonPattern)
	matches := re.FindStringSubmatch(response)
	
	if len(matches) > 1 {
		jsonContent := matches[1]
		if err := json.Unmarshal([]byte(jsonContent), &comments); err == nil {
			return comments, nil
		} else {
			return nil, fmt.Errorf("failed to parse extracted JSON: %v", err)
		}
	}

	// If no JSON block found, try to parse the entire response as a single comment
	return []CommentPayload{
		{
			Content: Content{
				Raw: response,
			},
		},
	}, nil
}

func isExoReviewerPresent(reviewers []struct {
	DisplayName string `json:"display_name"`
	UUID        string `json:"uuid"`
}) bool {
	for _, reviewer := range reviewers {
		// Check both display name and UUID since either might be used
		if reviewer.DisplayName == "ExoReview" || reviewer.UUID == "ExoReview" {
			return true
		}
	}
	return false
}

// hasJiraLink checks if the PR description contains a Jira ticket link
func hasJiraLink(description string) bool {
	// Common Jira ticket patterns
	patterns := []string{
		`[A-Z]+-\d+`,            // Basic Jira format like "PROJ-123"
		`jira\.exotel\.in/browse/[A-Z]+-\d+`, // Full Jira URL
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		if re.MatchString(description) {
			return true
		}
	}
	return false
}

// postJiraReminderComment posts a comment reminding to link a Jira ticket
func postJiraReminderComment(payload PullRequestCreatedPayload) error {
	// Get the first changed file from the PR
		comment := CommentPayload{
			Content: Content{
				Raw: "⚠️ This PR is not linked to any Jira ticket. Please update the PR description to include a Jira ticket reference for better tracking and documentation.",
			},
		}
		return postComment(comment, payload)
	}

func webhookHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Invalid request method", http.StatusMethodNotAllowed)
		return
	}

	eventKey := strings.TrimSpace(r.Header.Get("X-Event-Key"))
	log.Printf("Received X-Event-Key: '%s'", eventKey)

	if eventKey != "pullrequest:created" && eventKey != "pullrequest:updated" {
		log.Printf("Ignored event: %s", eventKey)
		w.WriteHeader(http.StatusOK)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Failed to read body: %v", err)
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var payload PullRequestCreatedPayload
	err = json.Unmarshal(body, &payload)
	if err != nil {
		log.Printf("Invalid JSON: %v", err)
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if len(payload.PullRequest.Reviewers) == 0 {
		log.Printf("PR #%d '%s' has no reviewers. Discarding.", payload.PullRequest.ID, payload.PullRequest.Title)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Check if exoReviewer is one of the reviewers
	if !isExoReviewerPresent(payload.PullRequest.Reviewers) {
		log.Printf("PR #%d '%s' does not have ExoReview assigned. Skipping review.", 
			payload.PullRequest.ID, payload.PullRequest.Title)
		w.WriteHeader(http.StatusOK)
		return
	}

	


	log.Printf("Accepted event for PR #%d '%s' with ExoReview assigned", 
		payload.PullRequest.ID, payload.PullRequest.Title)

	fetchAndDiff(
		payload.Repository.FullName,
		payload.PullRequest.Source.Branch.Name,
		payload.PullRequest.Destination.Branch.Name,
		payload,
	)

	// Read the generated diff file
	repoName := strings.ReplaceAll(strings.ReplaceAll(payload.Repository.FullName, "/", "_"), ".", "_")
	diffDir := "./diffs"
	timestamp := time.Now().Format("2006-01-02_15-04-05")
	diffFile := filepath.Join(diffDir, fmt.Sprintf("%s_%s_to_%s_%s.txt",
		repoName,
		strings.ReplaceAll(payload.PullRequest.Source.Branch.Name, "/", "_"),
		strings.ReplaceAll(payload.PullRequest.Destination.Branch.Name, "/", "_"),
		timestamp))

	diffContent, err := os.ReadFile(diffFile)
	if err != nil {
		log.Printf("Error reading diff file: %v", err)
		http.Error(w, "Failed to analyze PR", http.StatusInternalServerError)
		return
	}

	// Analyze the diff content with GPT-4
	analysis, err := analyzeWithGPT4(string(diffContent))
	if err != nil {
		log.Printf("Error analyzing PR with GPT-4: %v", err)
		http.Error(w, "Failed to analyze PR", http.StatusInternalServerError)
		return
	}

	// Log the complete GPT-4 response
	log.Printf("GPT-4 Analysis Response:\n%s\n", analysis)

	// Parse GPT-4 response into comments
	comments, err := parseGPT4Response(analysis)
	if err != nil {
		log.Printf("Error parsing GPT-4 analysis into comments: %v", err)
		log.Printf("Raw GPT-4 response that failed to parse:\n%s\n", analysis)
		http.Error(w, "Failed to process GPT-4 analysis", http.StatusInternalServerError)
		return
	}

	log.Printf("Successfully parsed %d comments from GPT-4", len(comments))

	// Track successful and failed comments
	successCount := 0
	failedCount := 0

	// Post each comment from GPT-4 analysis
	for i, comment := range comments {
		log.Printf("Posting comment %d/%d", i+1, len(comments))
		log.Printf("Comment content: %s", comment.Content.Raw)
		if comment.Inline != nil {
			log.Printf("File: %s, Line: %d", comment.Inline.Path, comment.Inline.To)
		}

		if err := postComment(comment, payload); err != nil {
			log.Printf("Error posting comment %d: %v", i+1, err)
			failedCount++
			continue
		}
		log.Printf("Successfully posted comment %d/%d to PR", i+1, len(comments))
		successCount++
	}

	// Log final statistics
	log.Printf("Comment posting complete. Success: %d, Failed: %d, Total: %d", 
		successCount, failedCount, len(comments))

	if failedCount > 0 {
		log.Printf("Warning: %d comments failed to post", failedCount)
	}

	w.WriteHeader(http.StatusOK)
}

func main() {
	http.HandleFunc("/webhook", webhookHandler)
	log.Println("Listening on :8080 for Bitbucket PR webhooks...")
	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
