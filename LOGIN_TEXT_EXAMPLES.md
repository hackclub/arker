# LOGIN_TEXT Environment Variable Examples

The `LOGIN_TEXT` environment variable allows you to display helpful text under the login form. This is especially useful for demo instances where you want to show users the credentials.

## Usage

Set the `LOGIN_TEXT` environment variable when starting the application:

```bash
# Simple credentials
export LOGIN_TEXT="Demo credentials: admin/admin"
./arker

# More detailed information
export LOGIN_TEXT="Demo instance - Username: admin, Password: admin"
./arker

# With HTML formatting
export LOGIN_TEXT="Use <strong>admin</strong> / <strong>admin</strong>"
./arker

# Multiline instructions
export LOGIN_TEXT="Demo Instance
Username: admin
Password: admin

This is a read-only demo environment."
./arker
```

## Docker Example

```dockerfile
# In your Dockerfile or docker-compose.yml
ENV LOGIN_TEXT="Demo Environment\nCredentials: admin / admin\nAll data is reset daily"
```

## Docker Compose Example

```yaml
version: '3.8'
services:
  arker:
    image: your-arker-image
    environment:
      - LOGIN_TEXT=Demo instance - Use admin/admin to login
    ports:
      - "8080:8080"
```

## Visual Result

When `LOGIN_TEXT` is set, a styled box appears under the login form containing your text:

```
┌─────────────────────────────┐
│        Arker Login          │
├─────────────────────────────┤
│ [Username: ______________ ] │
│ [Password: ______________ ] │
│ [      Login Button      ]  │
├─────────────────────────────┤
│ ┌─────────────────────────┐ │
│ │  Demo credentials:      │ │
│ │  admin/admin           │ │
│ └─────────────────────────┘ │
└─────────────────────────────┘
```

## Use Cases

- **Demo instances**: Show credentials to visitors
- **Development environments**: Provide test account information
- **Temporary instances**: Display access instructions
- **Onboarding**: Guide new users with setup information
- **Maintenance notices**: Inform users about system status

## Security Note

Only use `LOGIN_TEXT` for demo or development environments. Never display real credentials in production systems.
