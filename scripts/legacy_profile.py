"""Guard for anonymized, fixed-profile historical deployment tools."""
import os


def require_configured():
    if os.environ.get('ANANAS_LEGACY_PROFILE_CONFIGURED') != 'yes':
        raise SystemExit(
            'This is a historical template with fictitious deployment settings. '
            'Use install_ananas.py for normal setup. To run a reviewed, adapted '
            'template, explicitly set ANANAS_LEGACY_PROFILE_CONFIGURED=yes.')
