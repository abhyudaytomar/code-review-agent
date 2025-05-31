# 🤖 exoReviewer – AI-Powered PR Review for Bitbucket

**exoReviewer** is an automated code review assistant that uses LLMs (Large Language Models) to review Pull Requests on Bitbucket. It intelligently adds inline comments, checks for test cases, validates Jira linkage, and speeds up the code review process with high-quality feedback.

---

## 🚀 Features

- ✅ **LLM-Powered Code Review**  
  Generates human-like review comments based on Git diffs.

- 🧪 **Test File Detection**  
  Warns if changes lack associated test files.

- 🔗 **Jira Integration**  
  Validates Jira ticket linkage and provides context from ticket description.

- 🛠️ **Bitbucket Integration**  
  Automatically comments on PR when `exoReviewer` is added as a reviewer.

---

## 🧠 How It Works

1. **Trigger**: A PR is created on Bitbucket and `exoReviewer` is added as a reviewer.
2. **Webhook** fires and starts the backend pipeline.
3. **Clone the Repository** at the given commit state.
4. **Generate Git Diff** of the PR changes.
5. **Test Check**:
   - Check if any test files are included in the PR.
6. **LLM Interaction**:
   - Sends the diff, test check result, and Jira description to the LLM.
   - Receives back structured review comments.
7. **Post Comments** to the Bitbucket PR inline using Bitbucket API.

---

## 📁 Folder Structure
