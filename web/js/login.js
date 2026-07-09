document.addEventListener('DOMContentLoaded', () => {
    const form = document.getElementById('login-form');
    const emailInput = document.getElementById('email');
    const passwordInput = document.getElementById('password');
    const errorDisplay = document.getElementById('error-message');
    const submitBtn = form.querySelector('button[type="submit"]');
    const ssoSection = document.getElementById('sso-section');

    // Check if already logged in, redirect if so
    API.auth.me().then(() => {
        window.location.href = '/';
    }).catch(() => {
        // Not logged in, stay here
    });

    // Show the running build version under the login card
    renderAppVersion('app-version');

    // Check OIDC config and show SSO button if enabled
    fetch('/api/auth/oidc/config')
        .then(res => res.json())
        .then(config => {
            if (config.enabled && ssoSection) {
                ssoSection.classList.add('visible');
            }
        })
        .catch(() => {
            // OIDC not available, keep hidden
        });

    form.addEventListener('submit', async (e) => {
        e.preventDefault();

        const email = emailInput.value.trim();
        const password = passwordInput.value;

        if (!email || !password) return;

        submitBtn.disabled = true;
        submitBtn.textContent = 'Signing in...';
        errorDisplay.style.display = 'none';

        try {
            await API.auth.login(email, password);
            window.location.href = '/';
        } catch (error) {
            console.error('Login failed:', error);
            errorDisplay.textContent = error.message || 'Invalid email or password';
            errorDisplay.style.display = 'block';
            submitBtn.disabled = false;
            submitBtn.textContent = 'Sign In';
        }
    });
});
